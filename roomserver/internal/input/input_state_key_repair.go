// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"context"
	"fmt"
	"slices"

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
			roomID, rerr := r.repairRoomStateKeys(ctx, roomNID, byRoom[roomNID])
			if rerr != nil {
				// The batch is not recorded, so the next start checks it again.
				logrus.WithError(rerr).WithField("room_nid", roomNID).Warn("Failed to repair state events stored under the wrong state key")
				return
			}
			repaired[roomNID] += len(byRoom[roomNID])
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
	for roomNID, count := range repaired {
		total += count
		logrus.WithFields(logrus.Fields{
			"room_id": roomIDs[roomNID],
			"events":  count,
		}).Warn("Stored state events of the room under their own state key. " +
			"Events that were rejected because of the wrong keys stay rejected: " +
			"to restore the members they excluded, download the room state with " +
			"GET /_zendrite/admin/downloadState/{serverName}/{roomID}")
	}
	logrus.WithFields(logrus.Fields{
		"rooms":  len(repaired),
		"events": total,
	}).Info("Finished the state key repair")
}

// repairRoomStateKeys stores the given events of a room under their own state
// key and brings the memberships they affect in line with the room's current
// state, in one transaction. It returns the room ID.
func (r *Inputer) repairRoomStateKeys(
	ctx context.Context, roomNID types.RoomNID, miskeyed []shared.MiskeyedStateEvent,
) (roomID string, err error) {
	roomInfo, err := r.DB.RoomInfoByNID(ctx, roomNID)
	if err != nil {
		return "", fmt.Errorf("r.DB.RoomInfoByNID: %w", err)
	}
	if roomInfo == nil || roomInfo.IsStub() {
		return "", nil
	}

	// Assign the NIDs of state keys outside the room transaction, since the
	// state key NID cache is not rolled back with it.
	newStateKeyNIDs := make([]types.EventStateKeyNID, len(miskeyed))
	for i := range miskeyed {
		if newStateKeyNIDs[i], err = r.DB.GetOrCreateEventStateKeyNID(ctx, &miskeyed[i].StateKey); err != nil {
			return "", fmt.Errorf("r.DB.GetOrCreateEventStateKeyNID: %w", err)
		}
	}

	// In a room in partial state the membership table follows the partial
	// state, and the resync that completes it brings it up to date.
	partial, err := r.DB.IsRoomPartialState(ctx, roomNID)
	if err != nil {
		return "", fmt.Errorf("r.DB.IsRoomPartialState: %w", err)
	}

	var succeeded bool
	updater, err := r.DB.GetRoomUpdater(ctx, roomInfo)
	if err != nil {
		return "", fmt.Errorf("r.DB.GetRoomUpdater: %w", err)
	}
	defer sqlutil.EndTransactionWithCheck(updater, &succeeded, &err)

	eventNIDs := make([]types.EventNID, 0, len(miskeyed))
	memberKeys := map[types.EventStateKeyNID]struct{}{}
	for i, ev := range miskeyed {
		if err = updater.SetEventStateKeyNID(ev.EventNID, newStateKeyNIDs[i]); err != nil {
			return "", fmt.Errorf("updater.SetEventStateKeyNID: %w", err)
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
		return "", fmt.Errorf("updater.Events: %w", err)
	}
	if len(events) == 1 {
		roomID = events[0].RoomID().String()
	}

	var updates []api.OutputEvent
	if !partial && len(memberKeys) > 0 {
		// The current state now holds each repaired membership under its
		// own key, and the membership of the user whose key it was under
		// under theirs. Where a repaired event now shares its key with
		// another event in the snapshot, the one deeper in the room DAG
		// wins, so a repaired join never overrides a later leave or ban.
		// Record those as the users' memberships.
		roomState := state.NewStateResolution(updater, roomInfo, r.Queryer)
		var current []types.StateEntry
		current, err = roomState.LoadStateAtSnapshot(ctx, updater.CurrentStateSnapshotNID())
		if err != nil {
			return "", fmt.Errorf("roomState.LoadStateAtSnapshot: %w", err)
		}
		var candidates []types.StateEntry
		candidateNIDs := []types.EventNID{}
		for _, entry := range current {
			if entry.EventTypeNID != types.MRoomMemberNID {
				continue
			}
			if _, ok := memberKeys[entry.EventStateKeyNID]; ok {
				candidates = append(candidates, entry)
				candidateNIDs = append(candidateNIDs, entry.EventNID)
			}
		}
		// A rejected membership in the current state must not be recorded
		// as the user's membership.
		var candidateEvents []types.Event
		candidateEvents, err = updater.Events(ctx, roomInfo.RoomVersion, candidateNIDs)
		if err != nil {
			return "", fmt.Errorf("updater.Events: %w", err)
		}
		rejected := map[types.EventNID]bool{}
		for _, ev := range candidateEvents {
			if rejected[ev.EventNID], err = r.DB.IsEventRejected(ctx, roomNID, ev.EventID()); err != nil {
				return "", fmt.Errorf("r.DB.IsEventRejected: %w", err)
			}
		}
		var members []types.StateEntry
		for _, entry := range candidates {
			if !rejected[entry.EventNID] {
				members = append(members, entry)
			}
		}
		if updates, _, err = r.updateMemberships(ctx, updater, nil, members); err != nil {
			return "", fmt.Errorf("r.updateMemberships: %w", err)
		}
	}

	if len(updates) > 0 && roomID != "" {
		if err = r.OutputProducer.ProduceRoomEvents(roomID, updates); err != nil {
			return "", fmt.Errorf("r.OutputProducer.ProduceRoomEvents: %w", err)
		}
	}
	succeeded = true
	return roomID, nil
}
