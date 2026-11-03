// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/storage/shared"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

const (
	// RemoteStateServers is how many other servers must agree on a
	// membership before the repair takes it from their state.
	remoteStateServers = 2
	// RemoteStateTimeout bounds each /state_ids request.
	remoteStateTimeout = time.Minute
	// RemoteStateMaxServers bounds how many servers are asked in a room.
	remoteStateMaxServers = 6
)

// repairMembershipsFromRemoteState asks the servers of the room's moderators
// for its state and adds to the current state the memberships of the given
// users that they agree on, where the local repair found a later membership
// but could not add it: a rejoin that only passed with an authorization the
// room no longer holds, or two branches of memberships from the current one.
//
// Only servers with a joined user who may kick and ban under the current
// power levels are asked, since the membership skips the auth check against
// the current state a new event gets. A server anyone can run could otherwise
// vouch for an unban it forged against an old power levels event, and bring a
// banned user back. A moderator's server could make that change itself. At
// least two of them must answer, every one that answers must hold the same
// event for the user, this server must store and have accepted it, and it
// must follow the membership the current state holds, through the membership
// each event of the chain cites among its auth events. The servers can only
// move a user forward along a history this server holds and accepted; they
// cannot bring in events or roll a membership back. It returns how many
// memberships it added.
func (r *Inputer) repairMembershipsFromRemoteState(
	ctx context.Context, roomNID types.RoomNID, roomID string, keys map[types.EventStateKeyNID]struct{},
) (int, error) {
	if r.FSAPI == nil {
		return 0, nil
	}
	latestIDs, _, _, err := r.DB.LatestEventIDs(ctx, roomNID)
	if err != nil {
		return 0, fmt.Errorf("r.DB.LatestEventIDs: %w", err)
	}
	if len(latestIDs) == 0 {
		return 0, nil
	}
	servers, err := r.moderatorServers(ctx, roomID)
	if err != nil {
		return 0, err
	}

	// The membership event each server holds for each of the users.
	var views []map[types.EventStateKeyNID]types.EventNID
	for _, server := range servers {
		if ctx.Err() != nil {
			break
		}
		view, verr := r.remoteMembershipView(ctx, server, roomID, latestIDs, keys)
		if verr != nil {
			logrus.WithError(verr).WithFields(logrus.Fields{
				"room_id": roomID,
				"server":  server,
			}).Info("Could not get the room's state from a server for the membership repair")
			continue
		}
		views = append(views, view)
	}
	if len(views) < remoteStateServers {
		return 0, nil
	}
	agreed := map[types.EventStateKeyNID]types.EventNID{}
	for key, eventNID := range views[0] {
		if slices.IndexFunc(views[1:], func(view map[types.EventStateKeyNID]types.EventNID) bool {
			return view[key] != eventNID
		}) < 0 {
			agreed[key] = eventNID
		}
	}
	if len(agreed) == 0 {
		return 0, nil
	}
	return r.addAgreedMemberships(ctx, roomNID, roomID, agreed)
}

// moderatorServers returns the servers, other than this one, of the users
// joined to the room who may kick and ban under its current power levels,
// those of the most powerful users first, at most remoteStateMaxServers.
func (r *Inputer) moderatorServers(ctx context.Context, roomID string) ([]spec.ServerName, error) {
	plEvent, err := r.DB.GetStateEvent(ctx, roomID, spec.MRoomPowerLevels, "")
	if err != nil {
		return nil, fmt.Errorf("r.DB.GetStateEvent: %w", err)
	}
	if plEvent == nil {
		return nil, nil
	}
	powerLevels, err := plEvent.PowerLevels()
	if err != nil {
		return nil, fmt.Errorf("plEvent.PowerLevels: %w", err)
	}
	parsedRoomID, err := spec.NewRoomID(roomID)
	if err != nil {
		return nil, fmt.Errorf("spec.NewRoomID: %w", err)
	}
	threshold := max(powerLevels.Ban, powerLevels.Kick)
	levels := map[spec.ServerName]int64{}
	for senderID, level := range powerLevels.Users {
		if level < threshold {
			continue
		}
		member, merr := r.DB.GetStateEvent(ctx, roomID, spec.MRoomMember, senderID)
		if merr != nil {
			return nil, fmt.Errorf("r.DB.GetStateEvent: %w", merr)
		}
		if member == nil || gjson.GetBytes(member.Content(), "membership").Str != spec.Join {
			continue
		}
		userID, uerr := r.Queryer.QueryUserIDForSender(ctx, *parsedRoomID, spec.SenderID(senderID))
		if uerr != nil || userID == nil || r.Cfg.Matrix.IsLocalServerName(userID.Domain()) {
			continue
		}
		levels[userID.Domain()] = max(levels[userID.Domain()], level)
	}
	servers := slices.Collect(maps.Keys(levels))
	slices.SortFunc(servers, func(a, b spec.ServerName) int {
		if c := cmp.Compare(levels[b], levels[a]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	if len(servers) > remoteStateMaxServers {
		servers = servers[:remoteStateMaxServers]
	}
	return servers, nil
}

// remoteMembershipView returns the membership events the given server holds
// in the room's state for the given users, among those this server stores,
// asking for the state before one of the room's latest events.
func (r *Inputer) remoteMembershipView(
	ctx context.Context, server spec.ServerName, roomID string, latestIDs []string,
	keys map[types.EventStateKeyNID]struct{},
) (map[types.EventStateKeyNID]types.EventNID, error) {
	var lastErr error
	for _, eventID := range latestIDs {
		reqCtx, cancel := context.WithTimeout(ctx, remoteStateTimeout)
		res, err := r.FSAPI.LookupStateIDs(reqCtx, r.Cfg.Matrix.ServerName, server, roomID, eventID)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		entries, err := r.DB.StateEntriesForEventIDs(ctx, res.GetStateEventIDs(), true)
		if err != nil {
			return nil, fmt.Errorf("r.DB.StateEntriesForEventIDs: %w", err)
		}
		view := map[types.EventStateKeyNID]types.EventNID{}
		for _, entry := range entries {
			if _, ok := keys[entry.EventStateKeyNID]; ok && entry.EventTypeNID == types.MRoomMemberNID {
				view[entry.EventStateKeyNID] = entry.EventNID
			}
		}
		return view, nil
	}
	return nil, fmt.Errorf("r.FSAPI.LookupStateIDs: %w", lastErr)
}

// addAgreedMemberships replaces the memberships the current state holds for
// the given users with the given ones, where those follow them, in one room
// transaction, and announces the changes.
func (r *Inputer) addAgreedMemberships(
	ctx context.Context, roomNID types.RoomNID, roomID string, agreed map[types.EventStateKeyNID]types.EventNID,
) (added int, err error) {
	roomInfo, err := r.DB.RoomInfoByNID(ctx, roomNID)
	if err != nil {
		return 0, fmt.Errorf("r.DB.RoomInfoByNID: %w", err)
	}
	if roomInfo == nil || roomInfo.IsStub() {
		return 0, nil
	}
	var succeeded bool
	updater, err := r.DB.GetRoomUpdater(ctx, roomInfo)
	if err != nil {
		return 0, fmt.Errorf("r.DB.GetRoomUpdater: %w", err)
	}
	defer sqlutil.EndTransactionWithCheck(updater, &succeeded, &err)

	roomState := state.NewStateResolution(updater, roomInfo, r.Queryer)
	current, err := roomState.LoadStateAtSnapshot(ctx, updater.CurrentStateSnapshotNID())
	if err != nil {
		return 0, fmt.Errorf("roomState.LoadStateAtSnapshot: %w", err)
	}
	currentByKey := map[types.EventStateKeyNID]types.EventNID{}
	for _, entry := range current {
		if entry.EventTypeNID == types.MRoomMemberNID {
			currentByKey[entry.EventStateKeyNID] = entry.EventNID
		}
	}

	replace := map[types.EventNID]types.StateEntry{}
	var heads []types.StateEntry
	memberKeys := map[types.EventStateKeyNID]struct{}{}
	for key, eventNID := range agreed {
		currentNID, ok := currentByKey[key]
		if !ok || currentNID == eventNID {
			continue
		}
		follows, ferr := followsMembership(ctx, updater, roomInfo, eventNID, currentNID)
		if ferr != nil {
			return 0, ferr
		}
		if !follows {
			continue
		}
		head := types.StateEntry{
			StateKeyTuple: types.StateKeyTuple{EventTypeNID: types.MRoomMemberNID, EventStateKeyNID: key},
			EventNID:      eventNID,
		}
		replace[currentNID] = head
		heads = append(heads, head)
		memberKeys[key] = struct{}{}
	}
	if len(heads) == 0 {
		succeeded = true
		return 0, nil
	}
	stateBefore := updater.CurrentStateSnapshotNID()
	if err = replaceMemberships(ctx, updater, &roomState, roomNID, replace); err != nil {
		return 0, err
	}
	updates, err := acceptedMembershipOutputs(ctx, updater, &roomState, roomInfo, stateBefore, heads)
	if err != nil {
		return 0, err
	}
	membershipUpdates, err := r.updateRepairedMemberships(ctx, updater, &roomState, roomInfo, memberKeys)
	if err != nil {
		return 0, err
	}
	updates = append(updates, membershipUpdates...)
	if len(updates) > 0 {
		if err = r.OutputProducer.ProduceRoomEvents(roomID, updates); err != nil {
			return 0, fmt.Errorf("r.OutputProducer.ProduceRoomEvents: %w", err)
		}
	}
	succeeded = true
	logrus.WithFields(logrus.Fields{
		"room_id":     roomID,
		"memberships": len(heads),
	}).Warn("Added memberships the room's state on other servers holds")
	return len(heads), nil
}

// followsMembership reports whether the membership event with the given NID
// follows the one with the other NID, through the membership each event of
// the chain cites among its auth events, where every event of the chain is
// stored and accepted.
func followsMembership(
	ctx context.Context, updater *shared.RoomUpdater, roomInfo *types.RoomInfo, eventNID, ancestorNID types.EventNID,
) (bool, error) {
	nids := []types.EventNID{eventNID, ancestorNID}
	events, err := updater.Events(ctx, roomInfo.RoomVersion, nids)
	if err != nil {
		return false, fmt.Errorf("updater.Events: %w", err)
	}
	if len(events) != len(nids) {
		return false, nil
	}
	ev, ancestor := events[0], events[1]
	if ev.EventNID != eventNID {
		ev, ancestor = ancestor, ev
	}
	if ev.StateKey() == nil || ancestor.StateKey() == nil || *ev.StateKey() != *ancestor.StateKey() {
		return false, nil
	}
	for range maxJoinRun {
		rejected, rerr := updater.IsEventRejected(ctx, roomInfo.RoomNID, ev.EventID())
		if rerr != nil {
			return false, fmt.Errorf("updater.IsEventRejected: %w", rerr)
		}
		if rejected {
			return false, nil
		}
		authEvents, aerr := updater.EventsFromIDs(ctx, roomInfo, ev.AuthEventIDs())
		if aerr != nil {
			return false, fmt.Errorf("updater.EventsFromIDs: %w", aerr)
		}
		idx := slices.IndexFunc(authEvents, func(e types.Event) bool {
			return e.Type() == spec.MRoomMember && e.StateKey() != nil && *e.StateKey() == *ancestor.StateKey()
		})
		if idx < 0 {
			return false, nil
		}
		if authEvents[idx].EventNID == ancestor.EventNID {
			return true, nil
		}
		ev = authEvents[idx]
	}
	return false, nil
}
