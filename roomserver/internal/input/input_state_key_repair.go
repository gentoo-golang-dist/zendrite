// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/storage/shared"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// stateKeyRepairBatchSize is how many events the state key repair checks at
// a time.
const stateKeyRepairBatchSize = 1000

// RepairStateKeys stores state events that were stored under the wrong state
// key NID under their own state key, once, at startup.
//
// Before #445, auth events fetched over federation were stored under the state
// key of the event they authorized. A room's join rules fetched for a join
// were filed under the joining user's ID, and a membership fetched for a
// message under no state key at all. Snapshots that contain such an event miss
// it under its real key: a room whose join rules are missing is treated as
// invite-only, so every restricted join into it is rejected, and with it every
// later event of the user who joined.
//
// The repair checks every event that can be an auth event, in batches. A batch
// whose rooms were all repaired is recorded, so an interrupted repair resumes
// where it stopped on the next start.
func (r *Inputer) RepairStateKeys(ctx context.Context) {
	afterEventNID, pending, err := r.DB.StateKeyRepairCursor(ctx)
	if err != nil {
		logrus.WithError(err).Warn("Failed to read the progress of the state key repair")
		return
	}
	if !pending {
		return
	}
	logrus.WithField("after_event_nid", afterEventNID).Info("Checking for state events stored under the wrong state key")

	repaired := map[types.RoomNID]int{}
	resolved := map[types.RoomNID]int{}
	accepted := map[types.RoomNID]int{}
	acceptedEvents := map[types.RoomNID]int{}
	roomIDs := map[types.RoomNID]string{}
	for {
		if ctx.Err() != nil {
			return
		}
		miskeyed, lastEventNID, done, err := r.DB.MiskeyedStateEventsAfter(ctx, afterEventNID, stateKeyRepairBatchSize)
		if err != nil {
			logrus.WithError(err).Warn("Failed to check for state events stored under the wrong state key")
			return
		}
		byRoom := map[types.RoomNID][]shared.MiskeyedStateEvent{}
		for _, ev := range miskeyed {
			byRoom[ev.RoomNID] = append(byRoom[ev.RoomNID], ev)
		}
		roomNIDs := make([]types.RoomNID, 0, len(byRoom))
		for roomNID := range byRoom {
			roomNIDs = append(roomNIDs, roomNID)
		}
		slices.Sort(roomNIDs)
		for _, roomNID := range roomNIDs {
			roomID, resolvedHere, acceptedHere, acceptedEventsHere, rerr := r.repairRoomStateKeys(ctx, roomNID, byRoom[roomNID])
			if rerr == nil {
				// The events are accepted already, and any redaction that
				// arrived for them was applied to their stored JSON then. If
				// they cannot be announced, clients do not see them, since
				// the sync API serves only events it was told about.
				if aerr := r.announceAcceptedTimelineEvents(ctx, roomNID, roomID, acceptedEventsHere); aerr != nil {
					eventIDs := make([]string, len(acceptedEventsHere))
					for i := range acceptedEventsHere {
						eventIDs[i] = acceptedEventsHere[i].event.EventID()
					}
					logrus.WithError(aerr).WithFields(logrus.Fields{
						"room_id":   roomID,
						"event_ids": eventIDs,
					}).Error("Failed to announce events accepted by the state key repair; clients will not see them")
				}
			}
			if rerr != nil {
				// The batch is not recorded, so the next start checks it again.
				logrus.WithError(rerr).WithField("room_nid", roomNID).Warn("Failed to repair state events stored under the wrong state key")
				return
			}
			repaired[roomNID] += len(byRoom[roomNID])
			resolved[roomNID] += resolvedHere
			accepted[roomNID] += acceptedHere
			acceptedEvents[roomNID] += len(acceptedEventsHere)
			roomIDs[roomNID] = roomID
		}
		if err = r.DB.AdvanceStateKeyRepair(ctx, lastEventNID, done); err != nil {
			logrus.WithError(err).Warn("Failed to record the progress of the state key repair")
			return
		}
		if done {
			break
		}
		afterEventNID = lastEventNID
	}

	total := 0
	for roomNID, roomID := range roomIDs {
		total += repaired[roomNID]
		logrus.WithFields(logrus.Fields{
			"room_id":              roomID,
			"events":               repaired[roomNID],
			"resolved_snapshots":   resolved[roomNID],
			"accepted_memberships": accepted[roomNID],
			"accepted_events":      acceptedEvents[roomNID],
		}).Warn("Stored state events of the room under their own state key, and accepted the memberships " +
			"they had rejected and the events of those members that pass with them")
	}
	logrus.WithFields(logrus.Fields{
		"rooms":  len(roomIDs),
		"events": total,
	}).Info("Finished the state key repair")
}

// repairRoomStateKeys stores the given events of a room under their own state
// key, settles the keys this makes the room's current state and forward
// extremities hold twice, and brings the memberships the events affect in
// line with the current state, in one transaction. Memberships that were
// rejected because of the wrong keys and pass their checks now are accepted
// and added to the current state. It returns the room ID, how many snapshots
// it resolved, and how many memberships and other events it accepted.
func (r *Inputer) repairRoomStateKeys(
	ctx context.Context, roomNID types.RoomNID, miskeyed []shared.MiskeyedStateEvent,
) (roomID string, resolved, accepted int, acceptedEvents []acceptedTimelineEvent, err error) {
	roomInfo, err := r.DB.RoomInfoByNID(ctx, roomNID)
	if err != nil {
		return "", 0, 0, nil, fmt.Errorf("r.DB.RoomInfoByNID: %w", err)
	}
	if roomInfo == nil || roomInfo.IsStub() {
		return "", 0, 0, nil, nil
	}

	// Assign the NIDs of state keys outside the room transaction, since the
	// state key NID cache is not rolled back with it.
	newStateKeyNIDs := make([]types.EventStateKeyNID, len(miskeyed))
	for i := range miskeyed {
		if newStateKeyNIDs[i], err = r.DB.GetOrCreateEventStateKeyNID(ctx, &miskeyed[i].StateKey); err != nil {
			return "", 0, 0, nil, fmt.Errorf("r.DB.GetOrCreateEventStateKeyNID: %w", err)
		}
	}

	// In a room in partial state the current state and the membership table
	// are partial, and the resync that completes them replaces both.
	partial, err := r.DB.IsRoomPartialState(ctx, roomNID)
	if err != nil {
		return "", 0, 0, nil, fmt.Errorf("r.DB.IsRoomPartialState: %w", err)
	}

	var succeeded bool
	updater, err := r.DB.GetRoomUpdater(ctx, roomInfo)
	if err != nil {
		return "", 0, 0, nil, fmt.Errorf("r.DB.GetRoomUpdater: %w", err)
	}
	defer sqlutil.EndTransactionWithCheck(updater, &succeeded, &err)

	eventNIDs := make([]types.EventNID, 0, len(miskeyed))
	repairedNIDs := make(map[types.EventNID]struct{}, len(miskeyed))
	memberKeys := map[types.EventStateKeyNID]struct{}{}
	for i, ev := range miskeyed {
		repairedNIDs[ev.EventNID] = struct{}{}
		if err = updater.SetEventStateKeyNID(ev.EventNID, newStateKeyNIDs[i]); err != nil {
			return "", 0, 0, nil, fmt.Errorf("updater.SetEventStateKeyNID: %w", err)
		}
		eventNIDs = append(eventNIDs, ev.EventNID)
		if ev.EventTypeNID == types.MRoomMemberNID {
			memberKeys[newStateKeyNIDs[i]] = struct{}{}
			if ev.OldStateKeyNID != 0 {
				memberKeys[ev.OldStateKeyNID] = struct{}{}
			}
		}
	}

	events, err := updater.Events(ctx, roomInfo.RoomVersion, eventNIDs[:1])
	if err != nil {
		return "", 0, 0, nil, fmt.Errorf("updater.Events: %w", err)
	}
	if len(events) == 1 {
		roomID = events[0].RoomID().String()
	}

	var updates []api.OutputEvent
	if !partial {
		roomState := state.NewStateResolution(updater, roomInfo, r.Queryer)
		if resolved, err = resolveRepairedSnapshots(ctx, updater, &roomState, roomNID); err != nil {
			return "", 0, 0, nil, err
		}

		// Memberships rejected because of the wrong keys pass their checks
		// now. Accept them and add them to the state the room continues from.
		var acceptedEntries []types.StateEntry
		if acceptedEntries, err = r.recheckRejectedMemberships(ctx, updater, &roomState, roomInfo, repairedNIDs); err != nil {
			return "", 0, 0, nil, err
		}
		accepted = len(acceptedEntries)
		allAccepted := acceptedEntries
		if acceptedEntries, err = r.withoutSoftFailed(ctx, updater, &roomState, roomInfo, acceptedEntries); err != nil {
			return "", 0, 0, nil, err
		}
		if len(acceptedEntries) > 0 {
			stateBefore := updater.CurrentStateSnapshotNID()
			if err = addAcceptedMemberships(ctx, updater, &roomState, roomNID, acceptedEntries); err != nil {
				return "", 0, 0, nil, err
			}
			for _, entry := range acceptedEntries {
				memberKeys[entry.EventStateKeyNID] = struct{}{}
			}
			var outputs []api.OutputEvent
			if outputs, err = acceptedMembershipOutputs(ctx, updater, &roomState, roomInfo, stateBefore, acceptedEntries); err != nil {
				return "", 0, 0, nil, err
			}
			updates = append(updates, outputs...)
		}

		// The events their senders sent while the memberships were rejected
		// pass with them now. They were sent before any later change of the
		// memberships, so those left out of the current state count too.
		if len(allAccepted) > 0 {
			if acceptedEvents, err = r.recheckRejectedTimelineEvents(ctx, updater, &roomState, roomInfo, allAccepted); err != nil {
				return "", 0, 0, nil, err
			}
		}

		if len(memberKeys) > 0 {
			var membershipUpdates []api.OutputEvent
			if membershipUpdates, err = r.updateRepairedMemberships(ctx, updater, &roomState, roomInfo, memberKeys); err != nil {
				return "", 0, 0, nil, err
			}
			updates = append(updates, membershipUpdates...)
		}
	}

	if len(updates) > 0 && roomID != "" {
		if err = r.OutputProducer.ProduceRoomEvents(roomID, updates); err != nil {
			return "", 0, 0, nil, fmt.Errorf("r.OutputProducer.ProduceRoomEvents: %w", err)
		}
	}
	succeeded = true
	return roomID, resolved, accepted, acceptedEvents, nil
}

// resolveRepairedSnapshots settles the keys held twice in the room's current
// state and in the state before its forward extremities, which every new
// event's state is calculated from. A repaired event can share its key with
// the event that was the current state for it, and which of the two the room
// should hold is decided by state resolution, as for diverging branches. It
// returns how many snapshots it replaced.
func resolveRepairedSnapshots(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution, roomNID types.RoomNID,
) (int, error) {
	resolvedSnapshots := map[types.StateSnapshotNID]types.StateSnapshotNID{}
	resolve := func(stateNID types.StateSnapshotNID) (types.StateSnapshotNID, error) {
		if stateNID == 0 {
			return 0, nil
		}
		if done, ok := resolvedSnapshots[stateNID]; ok {
			return done, nil
		}
		entries, err := roomState.ResolveSnapshotConflicts(ctx, stateNID)
		if err != nil {
			return 0, fmt.Errorf("roomState.ResolveSnapshotConflicts: %w", err)
		}
		newNID := types.StateSnapshotNID(0)
		if entries != nil {
			if newNID, err = updater.AddState(ctx, roomNID, nil, entries); err != nil {
				return 0, fmt.Errorf("updater.AddState: %w", err)
			}
		}
		resolvedSnapshots[stateNID] = newNID
		return newNID, nil
	}

	replaced := 0
	newCurrent, err := resolve(updater.CurrentStateSnapshotNID())
	if err != nil {
		return 0, err
	}
	if newCurrent != 0 {
		if err = updater.SetCurrentStateSnapshotNID(newCurrent); err != nil {
			return 0, fmt.Errorf("updater.SetCurrentStateSnapshotNID: %w", err)
		}
		replaced++
	}
	for _, latest := range updater.LatestEvents() {
		newBefore, rerr := resolve(latest.BeforeStateSnapshotNID)
		if rerr != nil {
			return 0, rerr
		}
		if newBefore == 0 {
			continue
		}
		if err = updater.SetState(ctx, latest.EventNID, newBefore); err != nil {
			return 0, fmt.Errorf("updater.SetState: %w", err)
		}
		replaced++
	}
	return replaced, nil
}

// updateRepairedMemberships records, for each of the given users, the
// membership the room's current state holds for them. The current state now
// holds each repaired membership under its own key, and the membership of the
// user whose key it was stored under under theirs. A rejected membership is
// never recorded.
func (r *Inputer) updateRepairedMemberships(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution,
	roomInfo *types.RoomInfo, memberKeys map[types.EventStateKeyNID]struct{},
) ([]api.OutputEvent, error) {
	current, err := roomState.LoadStateAtSnapshot(ctx, updater.CurrentStateSnapshotNID())
	if err != nil {
		return nil, fmt.Errorf("roomState.LoadStateAtSnapshot: %w", err)
	}
	var candidates []types.StateEntry
	var candidateNIDs []types.EventNID
	for _, entry := range current {
		if entry.EventTypeNID != types.MRoomMemberNID {
			continue
		}
		if _, ok := memberKeys[entry.EventStateKeyNID]; ok {
			candidates = append(candidates, entry)
			candidateNIDs = append(candidateNIDs, entry.EventNID)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	candidateEvents, err := updater.Events(ctx, roomInfo.RoomVersion, candidateNIDs)
	if err != nil {
		return nil, fmt.Errorf("updater.Events: %w", err)
	}
	rejected := map[types.EventNID]bool{}
	for _, ev := range candidateEvents {
		if rejected[ev.EventNID], err = updater.IsEventRejected(ctx, roomInfo.RoomNID, ev.EventID()); err != nil {
			return nil, fmt.Errorf("updater.IsEventRejected: %w", err)
		}
	}
	var members []types.StateEntry
	for _, entry := range candidates {
		if !rejected[entry.EventNID] {
			members = append(members, entry)
		}
	}
	updates, _, err := r.updateMemberships(ctx, updater, nil, members)
	if err != nil {
		return nil, fmt.Errorf("r.updateMemberships: %w", err)
	}
	return updates, nil
}

// recheckRejectedMemberships checks again, in the order they were stored, the
// room's rejected membership events whose auth events or state before them
// include one of the given repaired events or a membership accepted here, and
// accepts those that pass against their auth events and the state before them,
// as when they arrived. A membership rejected because the state it was checked
// against missed events stored under the wrong key passes now; one rejected
// for another reason fails again. Outliers, which have no state of their own,
// are left alone. It returns the state entries of the accepted events.
func (r *Inputer) recheckRejectedMemberships(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution,
	roomInfo *types.RoomInfo, repaired map[types.EventNID]struct{},
) ([]types.StateEntry, error) {
	rejectedNIDs, err := updater.RejectedMembershipNIDs()
	if err != nil {
		return nil, fmt.Errorf("updater.RejectedMembershipNIDs: %w", err)
	}
	if len(rejectedNIDs) == 0 {
		return nil, nil
	}
	events, err := updater.Events(ctx, roomInfo.RoomVersion, rejectedNIDs)
	if err != nil {
		return nil, fmt.Errorf("updater.Events: %w", err)
	}
	slices.SortFunc(events, func(a, b types.Event) int { return cmp.Compare(a.EventNID, b.EventNID) })

	relevant := maps.Clone(repaired)
	var accepted []types.StateEntry
	for _, ev := range events {
		// Outliers have no state of their own to check them against.
		_, hasState, herr := stateAtEvents(ctx, updater, []string{ev.EventID()})
		if herr != nil {
			return nil, herr
		}
		if !hasState {
			continue
		}
		authEvents, aerr := acceptedAuthEvents(ctx, updater, roomInfo, ev.PDU)
		if aerr != nil {
			return nil, aerr
		}
		stateBefore, known, serr := stateBeforeForAuth(ctx, updater, roomState, ev.PDU)
		if serr != nil {
			return nil, serr
		}
		if !known || !dependsOn(authEvents, stateBefore, relevant) {
			continue
		}
		allowed, aerr := r.allowedBy(ctx, ev.PDU, pdusOf(authEvents))
		if aerr != nil {
			return nil, aerr
		}
		if allowed {
			if allowed, aerr = r.allowedByState(ctx, updater, roomInfo, ev.PDU, stateBefore); aerr != nil {
				return nil, aerr
			}
		}
		if !allowed {
			continue
		}
		if err = updater.SetEventUnrejected(ev.EventNID); err != nil {
			return nil, fmt.Errorf("updater.SetEventUnrejected: %w", err)
		}
		relevant[ev.EventNID] = struct{}{}
		entries, eerr := updater.StateEntriesForEventNIDs(ctx, []types.EventNID{ev.EventNID})
		if eerr != nil {
			return nil, fmt.Errorf("updater.StateEntriesForEventNIDs: %w", eerr)
		}
		accepted = append(accepted, entries...)
	}
	return accepted, nil
}

// dependsOn reports whether any of the auth events or state entries is one of
// the given events.
func dependsOn(authEvents []types.Event, stateEntries []types.StateEntry, events map[types.EventNID]struct{}) bool {
	for _, ev := range authEvents {
		if _, ok := events[ev.EventNID]; ok {
			return true
		}
	}
	for _, entry := range stateEntries {
		if _, ok := events[entry.EventNID]; ok {
			return true
		}
	}
	return false
}

// acceptedAuthEvents returns the auth events of an event that are not
// rejected, as stored in this transaction.
func acceptedAuthEvents(
	ctx context.Context, updater *shared.RoomUpdater, roomInfo *types.RoomInfo, event gomatrixserverlib.PDU,
) ([]types.Event, error) {
	authEvents, err := updater.EventsFromIDs(ctx, roomInfo, event.AuthEventIDs())
	if err != nil {
		return nil, fmt.Errorf("updater.EventsFromIDs: %w", err)
	}
	accepted := authEvents[:0]
	for _, authEvent := range authEvents {
		rejected, rerr := updater.IsEventRejected(ctx, roomInfo.RoomNID, authEvent.EventID())
		if rerr != nil {
			return nil, fmt.Errorf("updater.IsEventRejected: %w", rerr)
		}
		if !rejected {
			accepted = append(accepted, authEvent)
		}
	}
	return accepted, nil
}

// stateBeforeForAuth returns the entries of the state before an event that
// its auth needs, and the room's history visibility, from the state after its
// prev events. It returns false if the state at any of them is unknown.
func stateBeforeForAuth(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution, event gomatrixserverlib.PDU,
) ([]types.StateEntry, bool, error) {
	prevIDs := event.PrevEventIDs()
	if len(prevIDs) == 0 {
		return nil, false, nil
	}
	prevStates, known, err := stateAtEvents(ctx, updater, prevIDs)
	if err != nil || !known {
		return nil, false, err
	}
	tuples := gomatrixserverlib.StateNeededForAuth([]gomatrixserverlib.PDU{event}).Tuples()
	tuples = append(tuples, gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomHistoryVisibility, StateKey: ""})
	entries, err := roomState.LoadStateAfterEventsForStringTuples(ctx, prevStates, tuples)
	if err != nil {
		return nil, false, fmt.Errorf("roomState.LoadStateAfterEventsForStringTuples: %w", err)
	}
	return entries, true, nil
}

// stateAtEvents returns the state at the given events as stored in this
// transaction, and false if any of them is not stored or has no state of its
// own, as an outlier. The store reports an event without state as an error,
// and many rejected events are outliers.
func stateAtEvents(
	ctx context.Context, updater *shared.RoomUpdater, eventIDs []string,
) ([]types.StateAtEvent, bool, error) {
	states, err := updater.StateAtEventIDs(ctx, eventIDs)
	var missingState types.MissingStateError
	var missingEvent types.MissingEventError
	if errors.As(err, &missingState) || errors.As(err, &missingEvent) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("updater.StateAtEventIDs: %w", err)
	}
	if len(states) != len(eventIDs) {
		return nil, false, nil
	}
	for _, st := range states {
		if st.BeforeStateSnapshotNID == 0 {
			return nil, false, nil
		}
	}
	return states, true, nil
}

// allowedByState reports whether an event passes auth against the given state.
func (r *Inputer) allowedByState(
	ctx context.Context, updater *shared.RoomUpdater, roomInfo *types.RoomInfo,
	event gomatrixserverlib.PDU, entries []types.StateEntry,
) (bool, error) {
	nids := make([]types.EventNID, len(entries))
	for i, entry := range entries {
		nids[i] = entry.EventNID
	}
	events, err := updater.Events(ctx, roomInfo.RoomVersion, nids)
	if err != nil {
		return false, fmt.Errorf("updater.Events: %w", err)
	}
	return r.allowedBy(ctx, event, pdusOf(events))
}

// allowedBy reports whether an event passes auth against the given events.
func (r *Inputer) allowedBy(ctx context.Context, event gomatrixserverlib.PDU, authEvents []gomatrixserverlib.PDU) (bool, error) {
	provider, err := gomatrixserverlib.NewAuthEvents(nil)
	if err != nil {
		return false, fmt.Errorf("gomatrixserverlib.NewAuthEvents: %w", err)
	}
	for _, authEvent := range authEvents {
		if err = provider.AddEvent(authEvent); err != nil {
			return false, fmt.Errorf("provider.AddEvent: %w", err)
		}
	}
	return gomatrixserverlib.Allowed(event, provider, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
		return r.Queryer.QueryUserIDForSender(ctx, roomID, senderID)
	}) == nil, nil
}

func pdusOf(events []types.Event) []gomatrixserverlib.PDU {
	pdus := make([]gomatrixserverlib.PDU, len(events))
	for i := range events {
		pdus[i] = events[i].PDU
	}
	return pdus
}

// withoutSoftFailed returns the accepted memberships that also pass auth
// against the room's current state, which is what new events are checked
// against before they become part of it. A membership that fails it, for
// example a join of a user banned since, stays accepted but is not added to
// the current state.
func (r *Inputer) withoutSoftFailed(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution,
	roomInfo *types.RoomInfo, accepted []types.StateEntry,
) ([]types.StateEntry, error) {
	current, err := roomState.LoadStateAtSnapshot(ctx, updater.CurrentStateSnapshotNID())
	if err != nil {
		return nil, fmt.Errorf("roomState.LoadStateAtSnapshot: %w", err)
	}
	currentNIDs := make([]types.EventNID, len(current))
	for i, entry := range current {
		currentNIDs[i] = entry.EventNID
	}
	currentEvents, err := updater.Events(ctx, roomInfo.RoomVersion, currentNIDs)
	if err != nil {
		return nil, fmt.Errorf("updater.Events: %w", err)
	}
	currentPDUs := pdusOf(currentEvents)

	acceptedNIDs := make([]types.EventNID, len(accepted))
	for i, entry := range accepted {
		acceptedNIDs[i] = entry.EventNID
	}
	acceptedEvents, err := updater.Events(ctx, roomInfo.RoomVersion, acceptedNIDs)
	if err != nil {
		return nil, fmt.Errorf("updater.Events: %w", err)
	}
	passes := map[types.EventNID]bool{}
	for _, ev := range acceptedEvents {
		if passes[ev.EventNID], err = r.allowedBy(ctx, ev.PDU, currentPDUs); err != nil {
			return nil, err
		}
	}
	var result []types.StateEntry
	for _, entry := range accepted {
		if passes[entry.EventNID] {
			result = append(result, entry)
		}
	}
	return result, nil
}

// addAcceptedMemberships adds accepted membership events to the room's current
// state and to the state before each of its forward extremities. Where a
// state already holds a membership for the same user, state resolution
// decides between them.
func addAcceptedMemberships(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution,
	roomNID types.RoomNID, accepted []types.StateEntry,
) error {
	done := map[types.StateSnapshotNID]types.StateSnapshotNID{}
	withAccepted := func(stateNID types.StateSnapshotNID) (types.StateSnapshotNID, error) {
		if stateNID == 0 {
			return 0, nil
		}
		if newNID, ok := done[stateNID]; ok {
			return newNID, nil
		}
		entries, err := roomState.LoadStateAtSnapshot(ctx, stateNID)
		if err != nil {
			return 0, fmt.Errorf("roomState.LoadStateAtSnapshot: %w", err)
		}
		have := make(map[types.EventNID]struct{}, len(entries))
		for _, entry := range entries {
			have[entry.EventNID] = struct{}{}
		}
		for _, entry := range accepted {
			if _, ok := have[entry.EventNID]; !ok {
				entries = append(entries, entry)
			}
		}
		// Stored as one block, the accepted memberships and the ones the
		// state already holds for the same users are conflicts to resolve.
		newNID, err := updater.AddState(ctx, roomNID, nil, entries)
		if err != nil {
			return 0, fmt.Errorf("updater.AddState: %w", err)
		}
		resolved, err := roomState.ResolveSnapshotConflicts(ctx, newNID)
		if err != nil {
			return 0, fmt.Errorf("roomState.ResolveSnapshotConflicts: %w", err)
		}
		if resolved != nil {
			if newNID, err = updater.AddState(ctx, roomNID, nil, resolved); err != nil {
				return 0, fmt.Errorf("updater.AddState: %w", err)
			}
		}
		done[stateNID] = newNID
		return newNID, nil
	}

	newCurrent, err := withAccepted(updater.CurrentStateSnapshotNID())
	if err != nil {
		return err
	}
	if newCurrent != 0 {
		if err = updater.SetCurrentStateSnapshotNID(newCurrent); err != nil {
			return fmt.Errorf("updater.SetCurrentStateSnapshotNID: %w", err)
		}
	}
	latest, err := latestEventStates(ctx, updater)
	if err != nil {
		return err
	}
	for _, latest := range latest {
		newBefore, werr := withAccepted(latest.BeforeStateSnapshotNID)
		if werr != nil {
			return werr
		}
		if newBefore == 0 {
			continue
		}
		if err = updater.SetState(ctx, latest.EventNID, newBefore); err != nil {
			return fmt.Errorf("updater.SetState: %w", err)
		}
	}
	return nil
}

// acceptedMembershipOutputs tells the other components about the accepted
// memberships that are now in the room's current state. Each is sent as a new
// event that adds itself to the current state, replacing the membership the
// state held for the user before.
func acceptedMembershipOutputs(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution,
	roomInfo *types.RoomInfo, stateBefore types.StateSnapshotNID, accepted []types.StateEntry,
) ([]api.OutputEvent, error) {
	before, err := roomState.LoadStateAtSnapshot(ctx, stateBefore)
	if err != nil {
		return nil, fmt.Errorf("roomState.LoadStateAtSnapshot: %w", err)
	}
	after, err := roomState.LoadStateAtSnapshot(ctx, updater.CurrentStateSnapshotNID())
	if err != nil {
		return nil, fmt.Errorf("roomState.LoadStateAtSnapshot: %w", err)
	}
	beforeByKey := make(map[types.StateKeyTuple]types.EventNID, len(before))
	for _, entry := range before {
		beforeByKey[entry.StateKeyTuple] = entry.EventNID
	}
	afterByKey := make(map[types.StateKeyTuple]types.EventNID, len(after))
	for _, entry := range after {
		afterByKey[entry.StateKeyTuple] = entry.EventNID
	}

	var current []types.EventNID
	replaced := map[types.EventNID]types.EventNID{}
	for _, entry := range accepted {
		if afterByKey[entry.StateKeyTuple] != entry.EventNID {
			continue
		}
		current = append(current, entry.EventNID)
		if old, ok := beforeByKey[entry.StateKeyTuple]; ok && old != entry.EventNID {
			replaced[entry.EventNID] = old
		}
	}
	if len(current) == 0 {
		return nil, nil
	}

	historyVisibility := gomatrixserverlib.HistoryVisibilityShared
	lookup := slices.Clone(current)
	visibilityTuple := types.StateKeyTuple{EventTypeNID: types.MRoomHistoryVisibilityNID, EventStateKeyNID: types.EmptyStateKeyNID}
	visibilityNID, hasVisibility := afterByKey[visibilityTuple]
	if hasVisibility {
		lookup = append(lookup, visibilityNID)
	}
	for _, old := range replaced {
		lookup = append(lookup, old)
	}
	events, err := updater.Events(ctx, roomInfo.RoomVersion, lookup)
	if err != nil {
		return nil, fmt.Errorf("updater.Events: %w", err)
	}
	byNID := make(map[types.EventNID]types.Event, len(events))
	for _, ev := range events {
		byNID[ev.EventNID] = ev
	}
	if ev, ok := byNID[visibilityNID]; hasVisibility && ok {
		if visibility, verr := ev.HistoryVisibility(); verr == nil {
			historyVisibility = visibility
		}
	}

	latest := updater.LatestEvents()
	latestEventIDs := make([]string, len(latest))
	for i := range latest {
		latestEventIDs[i] = latest[i].EventID
	}
	var outputs []api.OutputEvent
	for _, eventNID := range current {
		ev, ok := byNID[eventNID]
		if !ok {
			return nil, fmt.Errorf("accepted membership %d is missing", eventNID)
		}
		ore := api.OutputNewRoomEvent{
			Event:             &types.HeaderedEvent{PDU: ev.PDU},
			LatestEventIDs:    latestEventIDs,
			LastSentEventID:   updater.LastEventIDSent(),
			AddsStateEventIDs: []string{ev.EventID()},
			SendAsServer:      string(api.DoNotSendToOtherServers),
			HistoryVisibility: historyVisibility,
		}
		if old, replacedOld := replaced[eventNID]; replacedOld {
			if oldEvent, found := byNID[old]; found {
				ore.RemovesStateEventIDs = []string{oldEvent.EventID()}
			}
		}
		outputs = append(outputs, api.OutputEvent{Type: api.OutputTypeNewRoomEvent, NewRoomEvent: &ore})
	}
	return outputs, nil
}

// latestEventStates returns the state at the room's forward extremities as
// stored in this transaction. The updater's latest events carry the state
// they had when the transaction started, which an earlier step may have
// replaced.
func latestEventStates(ctx context.Context, updater *shared.RoomUpdater) ([]types.StateAtEvent, error) {
	latest := updater.LatestEvents()
	ids := make([]string, len(latest))
	for i := range latest {
		ids[i] = latest[i].EventID
	}
	states, err := updater.StateAtEventIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("updater.StateAtEventIDs: %w", err)
	}
	return states, nil
}

// recheckRejectedTimelineEvents checks again the room's rejected timeline
// events that cite one of the given accepted memberships, their sender's own,
// as an auth event, and
// accepts those that pass. Their state before them was calculated while the
// membership was rejected, so they are checked against that state with the
// membership added, and where the state holds another membership for the
// sender, state resolution decides between them. Outliers, state events and
// redactions are left alone. Accepted events are announced as old events,
// which adds them to the room history without changing the current state, as
// for backfilled events, once the transaction is committed and redactions are
// applied to them. In a room with a policy server, an event must also be
// signed by it, as new events must; one it refuses, or that cannot be checked,
// stays rejected. It returns the accepted events.
func (r *Inputer) recheckRejectedTimelineEvents(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution,
	roomInfo *types.RoomInfo, acceptedMemberships []types.StateEntry,
) ([]acceptedTimelineEvent, error) {
	membershipNIDs := make([]types.EventNID, len(acceptedMemberships))
	entryByNID := make(map[types.EventNID]types.StateEntry, len(acceptedMemberships))
	for i, entry := range acceptedMemberships {
		membershipNIDs[i] = entry.EventNID
		entryByNID[entry.EventNID] = entry
	}
	memberships, err := updater.Events(ctx, roomInfo.RoomVersion, membershipNIDs)
	if err != nil {
		return nil, fmt.Errorf("updater.Events: %w", err)
	}
	type citedMembership struct {
		entry  types.StateEntry
		target string
	}
	membershipByID := make(map[string]citedMembership, len(memberships))
	for _, membership := range memberships {
		if membership.StateKey() != nil {
			membershipByID[membership.EventID()] = citedMembership{entryByNID[membership.EventNID], *membership.StateKey()}
		}
	}

	rejectedNIDs, err := updater.RejectedEventNIDs()
	if err != nil {
		return nil, fmt.Errorf("updater.RejectedEventNIDs: %w", err)
	}
	rejected, err := updater.Events(ctx, roomInfo.RoomVersion, rejectedNIDs)
	if err != nil {
		return nil, fmt.Errorf("updater.Events: %w", err)
	}
	slices.SortFunc(rejected, func(a, b types.Event) int { return cmp.Compare(a.EventNID, b.EventNID) })

	var accepted []acceptedTimelineEvent
	for _, ev := range rejected {
		if ev.StateKey() != nil || ev.Type() == spec.MRoomRedaction {
			continue
		}
		var cited []types.StateEntry
		for _, authEventID := range ev.AuthEventIDs() {
			if membership, ok := membershipByID[authEventID]; ok && membership.target == string(ev.SenderID()) {
				cited = append(cited, membership.entry)
			}
		}
		if len(cited) == 0 {
			continue
		}
		// Outliers have no state of their own to check them against.
		_, hasState, herr := stateAtEvents(ctx, updater, []string{ev.EventID()})
		if herr != nil {
			return nil, herr
		}
		if !hasState {
			continue
		}
		authEvents, aerr := acceptedAuthEvents(ctx, updater, roomInfo, ev.PDU)
		if aerr != nil {
			return nil, aerr
		}
		allowed, aerr := r.allowedBy(ctx, ev.PDU, pdusOf(authEvents))
		if aerr != nil {
			return nil, aerr
		}
		if !allowed {
			continue
		}
		entries, known, serr := stateBeforeForAuth(ctx, updater, roomState, ev.PDU)
		if serr != nil {
			return nil, serr
		}
		if !known {
			continue
		}
		if entries, serr = roomState.ResolveDuplicateKeys(ctx, append(entries, cited...)); serr != nil {
			return nil, fmt.Errorf("roomState.ResolveDuplicateKeys: %w", serr)
		}
		if allowed, aerr = r.allowedByState(ctx, updater, roomInfo, ev.PDU, entries); aerr != nil {
			return nil, aerr
		}
		if !allowed {
			continue
		}
		if _, perr := r.checkPolicyServer(ctx, roomInfo, ev.PDU); perr != nil {
			logrus.WithError(perr).WithField("event_id", ev.EventID()).Info("Leaving an event rejected that the room's policy server did not sign")
			continue
		}
		if err = updater.SetEventUnrejected(ev.EventNID); err != nil {
			return nil, fmt.Errorf("updater.SetEventUnrejected: %w", err)
		}
		visibility, verr := historyVisibilityIn(ctx, updater, roomInfo, entries)
		if verr != nil {
			return nil, verr
		}
		accepted = append(accepted, acceptedTimelineEvent{event: ev, historyVisibility: visibility})
	}
	return accepted, nil
}

// acceptedTimelineEvent is a timeline event the repair accepted, to announce
// once its transaction is committed.
type acceptedTimelineEvent struct {
	event             types.Event
	historyVisibility gomatrixserverlib.HistoryVisibility
}

// announceAcceptedTimelineEvents applies the redactions of the given accepted
// events, which may have arrived while they were rejected, and announces them
// as old events in their redacted form. It is called after the transaction
// that accepted them is committed, since applying redactions writes in a
// transaction of its own. An event whose redaction fails is not announced.
func (r *Inputer) announceAcceptedTimelineEvents(
	ctx context.Context, roomNID types.RoomNID, roomID string, accepted []acceptedTimelineEvent,
) error {
	if len(accepted) == 0 {
		return nil
	}
	roomInfo, err := r.DB.RoomInfoByNID(ctx, roomNID)
	if err != nil {
		return fmt.Errorf("r.DB.RoomInfoByNID: %w", err)
	}
	if roomInfo == nil {
		return nil
	}
	resolver := state.NewStateResolution(r.DB, roomInfo, r.Queryer)
	outputs := make([]api.OutputEvent, 0, len(accepted))
	for _, a := range accepted {
		event := a.event.PDU
		_, redacted, rerr := r.DB.MaybeRedactEvent(ctx, roomInfo, a.event.EventNID, event, &resolver, r.Queryer)
		if rerr != nil {
			logrus.WithError(rerr).WithField("event_id", event.EventID()).Warn("Not announcing an accepted event whose redactions could not be applied")
			continue
		}
		if redacted != nil && redacted.EventID() == event.EventID() {
			event = redacted
		}
		outputs = append(outputs, api.OutputEvent{
			Type: api.OutputTypeOldRoomEvent,
			OldRoomEvent: &api.OutputOldRoomEvent{
				Event:             &types.HeaderedEvent{PDU: event},
				HistoryVisibility: a.historyVisibility,
			},
		})
	}
	if len(outputs) == 0 {
		return nil
	}
	if err = r.OutputProducer.ProduceRoomEvents(roomID, outputs); err != nil {
		return fmt.Errorf("r.OutputProducer.ProduceRoomEvents: %w", err)
	}
	return nil
}

// historyVisibilityIn returns the history visibility the given state entries
// set, or shared if they set none.
func historyVisibilityIn(
	ctx context.Context, updater *shared.RoomUpdater, roomInfo *types.RoomInfo, entries []types.StateEntry,
) (gomatrixserverlib.HistoryVisibility, error) {
	for _, entry := range entries {
		if entry.EventTypeNID != types.MRoomHistoryVisibilityNID || entry.EventStateKeyNID != types.EmptyStateKeyNID {
			continue
		}
		events, err := updater.Events(ctx, roomInfo.RoomVersion, []types.EventNID{entry.EventNID})
		if err != nil {
			return "", fmt.Errorf("updater.Events: %w", err)
		}
		if len(events) == 1 {
			if visibility, verr := events[0].HistoryVisibility(); verr == nil {
				return visibility, nil
			}
		}
	}
	return gomatrixserverlib.HistoryVisibilityShared, nil
}
