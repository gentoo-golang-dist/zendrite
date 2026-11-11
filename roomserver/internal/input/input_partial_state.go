// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/storage/shared"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// The partial-state resync of MSC3902 works through the events whose state
// was calculated from partial state, oldest first, as Synapse does. For each
// event it calculates the state before the event from the full state after
// its prev events, checks the event against it again, and stores both in the
// same transaction as it removes the event's partial-state mark. Once no
// event is left, it calculates the room's current state from its forward
// extremities and clears the room's partial-state flag.
//
// Each step takes the room's input lock, so it never interleaves with an
// event of the room being processed.

// roomInputLocks serializes the processing of a room's input with the steps
// of its partial-state resync. Input is processed by one worker per room, so
// the lock is only ever contended by a resync.
var roomInputLocks sync.Map // room ID -> *sync.Mutex

func lockRoomInput(roomID string) func() {
	mu, _ := roomInputLocks.LoadOrStore(roomID, &sync.Mutex{})
	m := mu.(*sync.Mutex) //nolint:forcetypeassert // only mutexes are stored
	m.Lock()
	return m.Unlock
}

// isLocalJoin reports whether the event joins a local user to the room.
func (r *Inputer) isLocalJoin(ctx context.Context, event gomatrixserverlib.PDU) bool {
	if event.Type() != spec.MRoomMember || event.StateKey() == nil {
		return false
	}
	if membership, err := event.Membership(); err != nil || membership != spec.Join {
		return false
	}
	userID, err := r.Queryer.QueryUserIDForSender(ctx, event.RoomID(), spec.SenderID(*event.StateKey()))
	if err != nil || userID == nil {
		return false
	}
	return r.Cfg.Matrix.IsLocalServerName(userID.Domain())
}

// PartialStateResyncBatch returns up to limit events of the room that still
// have partial state and whose prev events do not, oldest first. The resync
// can work through them in any order.
func (r *Inputer) PartialStateResyncBatch(ctx context.Context, roomInfo *types.RoomInfo, limit int) ([]string, error) {
	const scan = 500
	nids, err := r.DB.PartialStateEventNIDs(ctx, roomInfo.RoomNID, scan)
	if err != nil {
		return nil, fmt.Errorf("r.DB.PartialStateEventNIDs: %w", err)
	}
	if len(nids) == 0 {
		return nil, nil
	}
	events, err := r.DB.Events(ctx, roomInfo.RoomVersion, nids)
	if err != nil {
		return nil, fmt.Errorf("r.DB.Events: %w", err)
	}
	partial := make(map[string]bool, len(events))
	for _, ev := range events {
		partial[ev.EventID()] = true
	}
	// A prev event outside the scanned events may still have partial state.
	var prevIDs []string
	for _, ev := range events {
		prevIDs = append(prevIDs, ev.PrevEventIDs()...)
	}
	prevMeta, err := r.DB.EventNIDs(ctx, prevIDs)
	if err != nil {
		return nil, fmt.Errorf("r.DB.EventNIDs: %w", err)
	}
	prevNIDs := make([]types.EventNID, 0, len(prevMeta))
	nidToID := make(map[types.EventNID]string, len(prevMeta))
	for id, meta := range prevMeta {
		prevNIDs = append(prevNIDs, meta.EventNID)
		nidToID[meta.EventNID] = id
	}
	partialPrevs, err := r.DB.PartialStateEvents(ctx, prevNIDs)
	if err != nil {
		return nil, fmt.Errorf("r.DB.PartialStateEvents: %w", err)
	}
	for _, nid := range partialPrevs {
		partial[nidToID[nid]] = true
	}

	var ready []string
	for _, ev := range events {
		blocked := false
		for _, prev := range ev.PrevEventIDs() {
			if partial[prev] {
				blocked = true
				break
			}
		}
		if !blocked {
			ready = append(ready, ev.EventID())
			if len(ready) >= limit {
				break
			}
		}
	}
	if len(ready) == 0 {
		// Every scanned event waits for another partial-state event, which
		// must then lie beyond the scan: take the oldest one so that the
		// resync makes progress, as the resync of its prev events will.
		return nil, fmt.Errorf("no partial-state event of room %d is ready among the oldest %d", roomInfo.RoomNID, len(events))
	}
	return ready, nil
}

// ResyncPartialStateEvent recalculates the state before a partial-state
// event from the full state after its prev events, checks the event against
// it again and removes its partial-state mark.
//
// The state after a prev event that this server has no full state for comes
// from statesBefore, which maps the prev event ID to the IDs of the state
// before it on another server (/state_ids); the prev event itself must be
// stored, at least as an outlier, and so must every listed state event. If a
// prev event is missing from statesBefore, its ID is returned in needState
// and nothing changes.
//
// It returns whether the event changed between accepted and rejected.
func (r *Inputer) ResyncPartialStateEvent(
	ctx context.Context, roomID string, roomInfo *types.RoomInfo, eventID string, statesBefore map[string][]string,
) (needState []string, rejectionChanged *bool, err error) {
	unlock := lockRoomInput(roomID)
	defer unlock()

	logger := logrus.WithFields(logrus.Fields{
		"room_id":  roomID,
		"event_id": eventID,
	})

	var succeeded bool
	updater, err := r.DB.GetRoomUpdater(ctx, roomInfo)
	if err != nil {
		return nil, nil, fmt.Errorf("r.DB.GetRoomUpdater: %w", err)
	}
	defer sqlutil.EndTransactionWithCheck(updater, &succeeded, &err)

	events, err := updater.EventsFromIDs(ctx, roomInfo, []string{eventID})
	if err != nil {
		return nil, nil, fmt.Errorf("updater.EventsFromIDs: %w", err)
	}
	if len(events) == 0 {
		return nil, nil, fmt.Errorf("partial-state event %s is not stored", eventID)
	}
	event := events[0]
	stillPartial, err := updater.PartialStateEvents([]types.EventNID{event.EventNID})
	if err != nil {
		return nil, nil, fmt.Errorf("updater.PartialStateEvents: %w", err)
	}
	if len(stillPartial) == 0 {
		succeeded = true
		return nil, nil, nil
	}

	roomState := state.NewStateResolution(updater, roomInfo, r.Queryer)

	// The state after each prev event: from this server where it has the
	// prev event's full state, and from statesBefore otherwise.
	var localPrevs []types.StateAtEvent
	var remoteEntries []types.StateEntry
	for _, prevID := range event.PrevEventIDs() {
		prevStates, serr := updater.StateAtEventIDs(ctx, []string{prevID})
		var missingEvent types.MissingEventError
		var missingState types.MissingStateError
		switch {
		case serr == nil && len(prevStates) == 1:
			partialPrev, perr := updater.PartialStateEvents([]types.EventNID{prevStates[0].EventNID})
			if perr != nil {
				return nil, nil, fmt.Errorf("updater.PartialStateEvents: %w", perr)
			}
			if len(partialPrev) > 0 {
				return nil, nil, fmt.Errorf("%w: %s", api.ErrPartialStatePrevEvents, prevID)
			}
			localPrevs = append(localPrevs, prevStates[0])
			continue
		case serr == nil, errors.As(serr, &missingEvent), errors.As(serr, &missingState):
		default:
			return nil, nil, fmt.Errorf("updater.StateAtEventIDs: %w", serr)
		}
		stateIDs, ok := statesBefore[prevID]
		if !ok {
			needState = append(needState, prevID)
			continue
		}
		entries, eerr := r.stateAfterRemotePrev(ctx, updater, prevID, stateIDs)
		if eerr != nil {
			return nil, nil, eerr
		}
		remoteEntries = append(remoteEntries, entries...)
	}
	if len(needState) > 0 {
		succeeded = true
		return needState, nil, nil
	}

	// The state before the event.
	var stateNID types.StateSnapshotNID
	if len(remoteEntries) == 0 {
		stateNID, err = roomState.CalculateAndStoreStateAfterEvents(ctx, localPrevs)
		if err != nil {
			return nil, nil, fmt.Errorf("roomState.CalculateAndStoreStateAfterEvents: %w", err)
		}
	} else {
		combined := remoteEntries
		if len(localPrevs) > 0 {
			var local []types.StateEntry
			local, err = roomState.LoadCombinedStateAfterEvents(ctx, localPrevs)
			if err != nil {
				return nil, nil, fmt.Errorf("roomState.LoadCombinedStateAfterEvents: %w", err)
			}
			combined = append(combined, local...)
		}
		combined = types.DeduplicateStateEntries(combined)
		combined, err = roomState.ResolveDuplicateKeys(ctx, combined)
		if err != nil {
			return nil, nil, fmt.Errorf("roomState.ResolveDuplicateKeys: %w", err)
		}
		stateNID, err = updater.AddState(ctx, roomInfo.RoomNID, nil, combined)
		if err != nil {
			return nil, nil, fmt.Errorf("updater.AddState: %w", err)
		}
	}

	// Check the event against its auth events and the state before it again.
	rejected, reason, err := r.recheckAgainstState(ctx, updater, &roomState, roomInfo, event.PDU, stateNID)
	if err != nil {
		return nil, nil, err
	}
	wasRejected, err := updater.IsEventRejected(ctx, roomInfo.RoomNID, eventID)
	if err != nil {
		return nil, nil, fmt.Errorf("updater.IsEventRejected: %w", err)
	}
	if rejected != wasRejected {
		if rejected {
			logger.WithField("reason", reason).Info("Rejecting an event accepted against partial state")
			err = updater.SetEventRejected(event.EventNID)
		} else {
			logger.Info("Accepting an event rejected against partial state")
			err = updater.SetEventUnrejected(event.EventNID)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("updating the rejection of %s: %w", eventID, err)
		}
		rejectionChanged = &rejected
	}

	if err = updater.SetState(ctx, event.EventNID, stateNID); err != nil {
		return nil, nil, fmt.Errorf("updater.SetState: %w", err)
	}
	if err = updater.UnmarkPartialStateEvent(event.EventNID); err != nil {
		return nil, nil, fmt.Errorf("updater.UnmarkPartialStateEvent: %w", err)
	}
	succeeded = true
	return nil, rejectionChanged, nil
}

// stateAfterRemotePrev returns the state entries after a prev event, from the
// IDs of the state before it on another server and the prev event itself.
func (r *Inputer) stateAfterRemotePrev(
	ctx context.Context, updater *shared.RoomUpdater, prevID string, stateIDs []string,
) ([]types.StateEntry, error) {
	present, err := updater.StateEntriesForEventIDs(ctx, stateIDs, false)
	if err != nil {
		return nil, fmt.Errorf("updater.StateEntriesForEventIDs: %w", err)
	}
	if distinct, incomplete := resyncStateIncomplete(stateIDs, len(present)); incomplete {
		return nil, fmt.Errorf(
			"%d of the %d state events before %s are not stored", distinct-len(present), distinct, prevID,
		)
	}
	entries, err := updater.StateEntriesForEventIDs(ctx, stateIDs, true)
	if err != nil {
		return nil, fmt.Errorf("updater.StateEntriesForEventIDs: %w", err)
	}
	prevEntries, err := updater.StateEntriesForEventIDs(ctx, []string{prevID}, true)
	if err != nil {
		return nil, fmt.Errorf("updater.StateEntriesForEventIDs: %w", err)
	}
	if len(prevEntries) == 0 || prevEntries[0].EventStateKeyNID == 0 {
		// A message, a rejected event, or one this server does not have:
		// the state after it is the state before it. The resync stores the
		// prev event before it asks for this, so it is never missing then.
		return types.DeduplicateStateEntries(entries), nil
	}
	merged := make([]types.StateEntry, 0, len(entries)+1)
	for _, entry := range entries {
		if entry.StateKeyTuple != prevEntries[0].StateKeyTuple {
			merged = append(merged, entry)
		}
	}
	merged = append(merged, prevEntries[0])
	return types.DeduplicateStateEntries(merged), nil
}

// recheckAgainstState checks the event against its own auth events and
// against the state before it, as when it was received.
func (r *Inputer) recheckAgainstState(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution,
	roomInfo *types.RoomInfo, event gomatrixserverlib.PDU, stateNID types.StateSnapshotNID,
) (rejected bool, reason error, err error) {
	if event.Type() == spec.MRoomCreate && event.StateKeyEquals("") {
		return false, nil, nil
	}
	querier := func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
		return r.Queryer.QueryUserIDForSender(ctx, roomID, senderID)
	}

	// Its own auth events.
	authStored, err := updater.EventsFromIDs(ctx, roomInfo, event.AuthEventIDs())
	if err != nil {
		return false, nil, fmt.Errorf("updater.EventsFromIDs: %w", err)
	}
	authPDUs := make([]gomatrixserverlib.PDU, 0, len(authStored))
	for _, ev := range authStored {
		if !ev.Rejected {
			authPDUs = append(authPDUs, ev.PDU)
		}
	}
	authEvents, err := gomatrixserverlib.NewAuthEvents(authPDUs)
	if err != nil {
		return true, err, nil
	}
	if reason = gomatrixserverlib.Allowed(event, authEvents, querier); reason != nil {
		return true, reason, nil
	}
	rejectedID, err := r.rejectedAuthEvent(ctx, updater, roomInfo, event)
	if err != nil {
		return false, nil, fmt.Errorf("r.rejectedAuthEvent: %w", err)
	}
	if rejectedID != "" {
		return true, fmt.Errorf("event cites the rejected auth event %s", rejectedID), nil
	}

	// The state before it.
	tuples := gomatrixserverlib.StateNeededForAuth([]gomatrixserverlib.PDU{event}).Tuples()
	entries, err := roomState.LoadStateAtSnapshotForStringTuples(ctx, stateNID, tuples)
	if err != nil {
		return false, nil, fmt.Errorf("roomState.LoadStateAtSnapshotForStringTuples: %w", err)
	}
	nids := make([]types.EventNID, 0, len(entries))
	for _, entry := range entries {
		nids = append(nids, entry.EventNID)
	}
	stateEvents, err := updater.Events(ctx, roomInfo.RoomVersion, nids)
	if err != nil {
		return false, nil, fmt.Errorf("updater.Events: %w", err)
	}
	statePDUs := make([]gomatrixserverlib.PDU, 0, len(stateEvents))
	for _, ev := range stateEvents {
		statePDUs = append(statePDUs, ev.PDU)
	}
	stateAuth, err := gomatrixserverlib.NewAuthEvents(statePDUs)
	if err != nil {
		return true, err, nil
	}
	if reason = gomatrixserverlib.Allowed(event, stateAuth, querier); reason != nil {
		return true, reason, nil
	}
	return false, nil, nil
}

// CompletePartialStateResync calculates the current state of a room whose
// partial-state events all have full state now, updates the memberships from
// it and clears the room's partial-state flag, all in one transaction. It
// reports false, and changes nothing, if partial-state events are left, as
// when one arrived since the resync last looked.
//
// It also returns the servers that local device list updates sent during the
// partial state may have missed, as Synapse works them out (MSC3902): the
// servers with a member who is joined now, or whose membership changed
// since the join, other than those the resident server named at the join,
// to which the updates were sent.
func (r *Inputer) CompletePartialStateResync(
	ctx context.Context, roomID string, roomInfo *types.RoomInfo,
) (res PartialStateResyncResult, err error) {
	res.LocalLeftJoin, res.DeviceListServers, res.LeftUsers, res.Cleared, err = r.completePartialStateResync(ctx, roomID, roomInfo)
	return res, err
}

// PartialStateResyncResult is the outcome of CompletePartialStateResync.
type PartialStateResyncResult struct {
	// Cleared reports whether the room has full state now.
	Cleared bool
	// LocalLeftJoin reports whether a local user's join left the state.
	LocalLeftJoin bool
	// DeviceListServers are the servers that local device list updates may
	// have missed.
	DeviceListServers []string
	// LeftUsers are the remote users whose join left the current state.
	LeftUsers []string
}

func (r *Inputer) completePartialStateResync(
	ctx context.Context, roomID string, roomInfo *types.RoomInfo,
) (localLeftJoin bool, deviceListServers, leftUsers []string, cleared bool, err error) {
	unlock := lockRoomInput(roomID)
	defer unlock()

	var succeeded bool
	updater, err := r.DB.GetRoomUpdater(ctx, roomInfo)
	if err != nil {
		return false, nil, nil, false, fmt.Errorf("r.DB.GetRoomUpdater: %w", err)
	}
	defer sqlutil.EndTransactionWithCheck(updater, &succeeded, &err)

	partial, err := updater.IsRoomPartialState(roomInfo.RoomNID)
	if err != nil {
		return false, nil, nil, false, fmt.Errorf("updater.IsRoomPartialState: %w", err)
	}
	if !partial {
		succeeded = true
		return false, nil, nil, true, nil
	}

	latest := updater.LatestEvents()
	var outputEvents []api.OutputEvent
	if len(latest) > 0 {
		latestNIDs := make([]types.EventNID, 0, len(latest))
		for _, l := range latest {
			latestNIDs = append(latestNIDs, l.EventNID)
		}
		var partialLatest []types.EventNID
		if partialLatest, err = updater.PartialStateEvents(latestNIDs); err != nil {
			return false, nil, nil, false, fmt.Errorf("updater.PartialStateEvents: %w", err)
		}
		if len(partialLatest) > 0 {
			succeeded = true
			return false, nil, nil, false, nil
		}

		// The latest events were loaded before their state was recalculated
		// in other transactions, so load it again.
		ids := make([]string, 0, len(latest))
		for _, l := range latest {
			ids = append(ids, l.EventID)
		}
		var stateAt []types.StateAtEvent
		if stateAt, err = updater.StateAtEventIDs(ctx, ids); err != nil {
			return false, nil, nil, false, fmt.Errorf("updater.StateAtEventIDs: %w", err)
		}

		roomState := state.NewStateResolution(updater, roomInfo, r.Queryer)
		oldStateNID := updater.CurrentStateSnapshotNID()
		var newStateNID types.StateSnapshotNID
		if newStateNID, err = roomState.CalculateAndStoreStateAfterEvents(ctx, stateAt); err != nil {
			return false, nil, nil, false, fmt.Errorf("roomState.CalculateAndStoreStateAfterEvents: %w", err)
		}
		var removed, added []types.StateEntry
		if removed, added, err = roomState.DifferenceBetweeenStateSnapshots(ctx, oldStateNID, newStateNID); err != nil {
			return false, nil, nil, false, fmt.Errorf("roomState.DifferenceBetweeenStateSnapshots: %w", err)
		}
		if len(removed) > 0 || len(added) > 0 {
			if outputEvents, localLeftJoin, err = r.updateMemberships(ctx, updater, removed, added); err != nil {
				return false, nil, nil, false, fmt.Errorf("r.updateMemberships: %w", err)
			}
			if leftUsers, err = r.remoteLeftUsers(ctx, removed); err != nil {
				return false, nil, nil, false, err
			}
		}
		if err = updater.SetCurrentStateSnapshotNID(newStateNID); err != nil {
			return false, nil, nil, false, fmt.Errorf("updater.SetCurrentStateSnapshotNID: %w", err)
		}
		if deviceListServers, err = r.missedDeviceListServers(ctx, updater, &roomState, roomInfo, newStateNID); err != nil {
			return false, nil, nil, false, err
		}
	}

	if cleared, err = updater.ClearRoomPartialState(roomInfo.RoomNID); err != nil {
		return false, nil, nil, false, fmt.Errorf("updater.ClearRoomPartialState: %w", err)
	}
	if !cleared {
		// An event with partial state arrived since the batch was taken.
		// Roll back so the current state is calculated once it is done.
		return false, nil, nil, false, nil
	}
	succeeded = true

	if len(outputEvents) > 0 {
		if err = r.OutputProducer.ProduceRoomEvents(roomID, outputEvents); err != nil {
			logrus.WithError(err).WithField("room_id", roomID).Error("Failed to produce output events after a partial-state resync")
		}
	}
	return localLeftJoin, deviceListServers, leftUsers, true, nil
}

// remoteLeftUsers returns the remote users whose join is among the removed
// state entries.
func (r *Inputer) remoteLeftUsers(ctx context.Context, removed []types.StateEntry) ([]string, error) {
	var nids []types.EventStateKeyNID
	for _, entry := range removed {
		if entry.EventTypeNID == types.MRoomMemberNID {
			nids = append(nids, entry.EventStateKeyNID)
		}
	}
	if len(nids) == 0 {
		return nil, nil
	}
	userIDs, err := r.DB.EventStateKeys(ctx, nids)
	if err != nil {
		return nil, fmt.Errorf("r.DB.EventStateKeys: %w", err)
	}
	left := make([]string, 0, len(userIDs))
	for _, userID := range userIDs {
		_, domain, serr := gomatrixserverlib.SplitID('@', userID)
		if serr != nil || r.Cfg.Matrix.IsLocalServerName(domain) {
			continue
		}
		left = append(left, userID)
	}
	return left, nil
}

// missedDeviceListServers returns the servers that local device list updates
// sent while the room had partial state may have missed.
func (r *Inputer) missedDeviceListServers(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution,
	roomInfo *types.RoomInfo, stateNID types.StateSnapshotNID,
) ([]string, error) {
	joinNID, err := updater.PartialStateJoinEventNID(roomInfo.RoomNID)
	if err != nil {
		return nil, fmt.Errorf("updater.PartialStateJoinEventNID: %w", err)
	}
	known, err := updater.PartialStateServers(roomInfo.RoomNID)
	if err != nil {
		return nil, fmt.Errorf("updater.PartialStateServers: %w", err)
	}
	entries, err := roomState.LoadStateAtSnapshot(ctx, stateNID)
	if err != nil {
		return nil, fmt.Errorf("roomState.LoadStateAtSnapshot: %w", err)
	}
	var memberNIDs []types.EventNID
	for _, entry := range entries {
		if entry.EventTypeNID == types.MRoomMemberNID {
			memberNIDs = append(memberNIDs, entry.EventNID)
		}
	}
	members, err := updater.Events(ctx, roomInfo.RoomVersion, memberNIDs)
	if err != nil {
		return nil, fmt.Errorf("updater.Events: %w", err)
	}
	skip := make(map[spec.ServerName]bool, len(known))
	for _, server := range known {
		skip[spec.ServerName(server)] = true
	}
	var servers []string
	for _, member := range members {
		membership, merr := member.Membership()
		if merr != nil || member.StateKey() == nil {
			continue
		}
		if membership != spec.Join && member.EventNID <= joinNID {
			continue
		}
		userID, uerr := r.Queryer.QueryUserIDForSender(ctx, member.RoomID(), spec.SenderID(*member.StateKey()))
		if uerr != nil || userID == nil {
			continue
		}
		server := userID.Domain()
		if skip[server] || r.Cfg.Matrix.IsLocalServerName(server) {
			continue
		}
		skip[server] = true
		servers = append(servers, string(server))
	}
	return servers, nil
}
