// Copyright 2025 Jackmaninov
// Copyright 2025 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/storage/shared"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// resyncStateIncomplete reports whether the state entries loaded for a resync
// fail to cover every distinct requested state event ID. The loaded argument is
// the number of entries StateEntriesForEventIDs returned for requestedIDs;
// because that query returns at most one row per (present, non-rejected) event
// ID, a loaded count below the number of *distinct* requested IDs means some
// events were missing from the DB or rejected. The requestedIDs slice is
// deduplicated first so that a repeated ID in the /state_ids response cannot be
// mistaken for a shortfall. It also returns the distinct requested count for
// logging. See issue #247.
func resyncStateIncomplete(requestedIDs []string, loaded int) (distinctRequested int, incomplete bool) {
	seen := make(map[string]struct{}, len(requestedIDs))
	for _, id := range requestedIDs {
		seen[id] = struct{}{}
	}
	return len(seen), loaded < len(seen)
}

// RepairResyncedRooms repairs the forward extremity state of rooms that
// completed a partial state resync before UpdateStateAfterResync repaired it
// itself. The guard against state regressions kept their current state
// correct, so it is what the extremities are filled in from. Each room is
// repaired once; a failure leaves it flagged for the next start.
func (r *Inputer) RepairResyncedRooms(ctx context.Context) {
	roomNIDs, err := r.DB.ResyncRepairPendingRooms(ctx)
	if err != nil {
		logrus.WithError(err).Warn("Failed to list rooms awaiting resync repair")
		return
	}
	if len(roomNIDs) == 0 {
		return
	}
	repairedRooms := 0
	for _, roomNID := range roomNIDs {
		if ctx.Err() != nil {
			return
		}
		repaired, rerr := r.repairResyncedRoom(ctx, roomNID)
		if rerr != nil {
			logrus.WithError(rerr).WithField("room_nid", roomNID).Warn("Failed to repair forward extremities of resynced room")
			continue
		}
		if repaired > 0 {
			repairedRooms++
		}
	}
	logrus.WithFields(logrus.Fields{
		"rooms":          len(roomNIDs),
		"repaired_rooms": repairedRooms,
	}).Info("Repaired forward extremities of previously resynced rooms")
}

// repairResyncedRoom fills in the state before the room's forward extremities
// from its current state and clears its repair flag, returning how many
// extremities it changed.
func (r *Inputer) repairResyncedRoom(ctx context.Context, roomNID types.RoomNID) (repaired int, err error) {
	roomInfo, err := r.DB.RoomInfoByNID(ctx, roomNID)
	if err != nil {
		return 0, fmt.Errorf("r.DB.RoomInfoByNID: %w", err)
	}
	if roomInfo == nil || roomInfo.IsStub() {
		return 0, nil
	}
	// A room back in partial state has a resync of its own coming, which
	// repairs the extremities from complete state; its current state is
	// partial now, so it must not be used.
	partial, err := r.DB.IsRoomPartialState(ctx, roomNID)
	if err != nil {
		return 0, fmt.Errorf("r.DB.IsRoomPartialState: %w", err)
	}

	var succeeded bool
	updater, err := r.DB.GetRoomUpdater(ctx, roomInfo)
	if err != nil {
		return 0, fmt.Errorf("r.DB.GetRoomUpdater: %w", err)
	}
	defer sqlutil.EndTransactionWithCheck(updater, &succeeded, &err)

	if !partial {
		roomState := state.NewStateResolution(updater, roomInfo, r.Queryer)
		var current []types.StateEntry
		current, err = roomState.LoadStateAtSnapshot(ctx, updater.CurrentStateSnapshotNID())
		if err != nil {
			return 0, fmt.Errorf("roomState.LoadStateAtSnapshot: %w", err)
		}
		repaired, err = repairExtremityState(ctx, updater, &roomState, roomNID, updater.LatestEvents(), current)
		if err != nil {
			return 0, fmt.Errorf("repairExtremityState: %w", err)
		}
	}
	if err = updater.ClearResyncRepairPending(roomNID); err != nil {
		return 0, fmt.Errorf("updater.ClearResyncRepairPending: %w", err)
	}
	succeeded = true
	return repaired, nil
}

// repairExtremityState fills in the state stored before each forward extremity
// with the resync state entries for keys it lacks, and returns how many
// extremities it changed. Keys the extremity already has are kept, since they
// can be newer than the resync state.
func repairExtremityState(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution,
	roomNID types.RoomNID, latestEvents []types.StateAtEventAndReference, resyncState []types.StateEntry,
) (int, error) {
	repaired := 0
	for _, latest := range latestEvents {
		if latest.BeforeStateSnapshotNID == 0 {
			continue
		}
		have, err := roomState.LoadStateAtSnapshot(ctx, latest.BeforeStateSnapshotNID)
		if err != nil {
			return repaired, fmt.Errorf("roomState.LoadStateAtSnapshot: %w", err)
		}
		missing := missingStateEntries(have, resyncState)
		if len(missing) == 0 {
			continue
		}
		blockLists, err := updater.StateBlockNIDs(ctx, []types.StateSnapshotNID{latest.BeforeStateSnapshotNID})
		if err != nil {
			return repaired, fmt.Errorf("updater.StateBlockNIDs: %w", err)
		}
		if len(blockLists) != 1 {
			return repaired, fmt.Errorf("expected one state block list for snapshot %d, got %d", latest.BeforeStateSnapshotNID, len(blockLists))
		}
		stateNID, err := updater.AddState(ctx, roomNID, blockLists[0].StateBlockNIDs, missing)
		if err != nil {
			return repaired, fmt.Errorf("updater.AddState: %w", err)
		}
		if err = updater.SetState(ctx, latest.EventNID, stateNID); err != nil {
			return repaired, fmt.Errorf("updater.SetState: %w", err)
		}
		repaired++
	}
	return repaired, nil
}

// missingStateEntries returns the entries of resyncState whose state keys do
// not appear in have.
func missingStateEntries(have, resyncState []types.StateEntry) []types.StateEntry {
	keys := make(map[types.StateKeyTuple]struct{}, len(have))
	for _, e := range have {
		keys[e.StateKeyTuple] = struct{}{}
	}
	var missing []types.StateEntry
	for _, e := range resyncState {
		if _, ok := keys[e.StateKeyTuple]; !ok {
			missing = append(missing, e)
		}
	}
	return missing
}
