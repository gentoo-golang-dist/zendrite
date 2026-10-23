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
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/internal/helpers"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/storage/shared"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// UpdateStateAfterResync updates the current state and memberships after a partial state resync.
// This is called after state events have been stored as outliers via SendStateAsOutliers.
// It creates a new state snapshot from the stored events, calculates the state delta,
// updates the membership table, and notifies downstream components (syncapi).
//
// StateEventIDs are the event IDs of the state events that were fetched during resync.
//
// Returns localLeftJoin=true if at least one local user transitioned out of join
// during this resync. The caller should invoke scheduleAutoPurgeIfEmpty after
// the transaction has committed (i.e. after this function returns successfully).
//
//nolint:gocyclo
func (r *Inputer) UpdateStateAfterResync(ctx context.Context, roomID string, stateEventIDs []string) (localLeftJoin bool, roomInfo *types.RoomInfo, err error) {
	logger := logrus.WithFields(logrus.Fields{
		"room_id":           roomID,
		"state_event_count": len(stateEventIDs),
		"trace":             "partial_state_resync",
	})
	logger.Info("Updating current state after partial state resync")

	// Get room info — assign into named return so the caller can use it.
	roomInfo, err = r.DB.RoomInfo(ctx, roomID)
	if err != nil {
		return false, nil, fmt.Errorf("r.DB.RoomInfo: %w", err)
	}
	if roomInfo == nil {
		return false, nil, fmt.Errorf("room %s not found", roomID)
	}

	// Non-rejected state entries form the snapshot we actually apply.
	var stateEntries []types.StateEntry
	stateEntries, err = r.DB.StateEntriesForEventIDs(ctx, stateEventIDs, true)
	if err != nil {
		return false, nil, fmt.Errorf("r.DB.StateEntriesForEventIDs: %w", err)
	}

	// Present entries (rejected included) measure completeness. The resync fetch
	// step guarantees every advertised state event is stored, so a shortfall
	// here means events are genuinely absent from the DB (a truncated fetch
	// worth retrying, issue #247) rather than merely rejected.
	var presentEntries []types.StateEntry
	presentEntries, err = r.DB.StateEntriesForEventIDs(ctx, stateEventIDs, false)
	if err != nil {
		return false, nil, fmt.Errorf("r.DB.StateEntriesForEventIDs (present): %w", err)
	}

	// Debug: Count EventTypeNIDs in loaded state entries
	loadedEventTypeNIDCounts := make(map[types.EventTypeNID]int)
	loadedMemberCount := 0
	for _, entry := range stateEntries {
		loadedEventTypeNIDCounts[entry.EventTypeNID]++
		if entry.EventTypeNID == types.MRoomMemberNID {
			loadedMemberCount++
		}
	}

	logger.WithFields(logrus.Fields{
		"state_entries":          len(stateEntries),
		"loaded_member_events":   loadedMemberCount,
		"loaded_type_nid_counts": loadedEventTypeNIDCounts,
	}).Debug("Loaded state entries from event IDs with EventTypeNID breakdown")

	// A resync must materialize every state event that /state_ids advertised
	// before we replace the room's current state with it. If any requested ID is
	// absent from the DB entirely (a fetch/store failure), the new snapshot (and
	// every count derived from it, such as joined members) would be truncated.
	// Committing that is exactly what leaves the member count stuck low and
	// clears the partial-state flag so it never self-corrects (issue #247).
	// Refuse to proceed: returning an error keeps the room in partial state and
	// lets the federation resync worker retry (against another server, with
	// backoff) instead of persisting a corrupt snapshot.
	//
	// Completeness is measured against *present* entries, not the non-rejected
	// snapshot: an event that is stored but rejected (an unverifiable signature)
	// is materialized as far as it ever will be, so it must not keep us looping.
	if distinctRequested, incomplete := resyncStateIncomplete(stateEventIDs, len(presentEntries)); incomplete {
		logger.WithFields(logrus.Fields{
			"requested_state_events": distinctRequested,
			"present_state_events":   len(presentEntries),
			"loaded_state_events":    len(stateEntries),
			"loaded_member_events":   loadedMemberCount,
		}).Error("Resync is missing state events entirely; refusing to apply truncated state")
		return false, roomInfo, fmt.Errorf(
			"resync state incomplete: %d of %d requested state events absent from the database",
			distinctRequested-len(presentEntries), distinctRequested,
		)
	}

	// Any requested events that are present but rejected cannot enter a
	// non-rejected snapshot. Step 2 of the resync already re-fetched them to
	// retry verification; those still rejected have a signature we cannot verify
	// (e.g. a signing key we cannot fetch) and are applied best-effort by
	// excluding them rather than looping on them forever.
	if rejectedExcluded := len(presentEntries) - len(stateEntries); rejectedExcluded > 0 {
		logger.WithField("rejected_state_events_excluded", rejectedExcluded).
			Warn("Applying resync state without locally-rejected state events; their signatures could not be verified")
	}

	if len(stateEntries) == 0 {
		// Nothing was requested (uniqueRequested is also empty); genuine no-op.
		logger.Warn("No state entries found for resync, skipping state update")
		return false, roomInfo, nil
	}

	// Deduplicate state entries (in case of duplicates)
	stateEntries = types.DeduplicateStateEntries(stateEntries)

	// Get the room updater (for transaction and locking)
	var succeeded bool
	var updater *shared.RoomUpdater
	updater, err = r.DB.GetRoomUpdater(ctx, roomInfo)
	if err != nil {
		return false, nil, fmt.Errorf("r.DB.GetRoomUpdater: %w", err)
	}
	defer sqlutil.EndTransactionWithCheck(updater, &succeeded, &err)

	// Get current state snapshot NID
	oldStateNID := updater.CurrentStateSnapshotNID()

	logger.WithField("old_state_nid", oldStateNID).Debug("Got old state snapshot NID")

	// MSC3706 Fix: Preserve local member events from the old state.
	// The remote server's /state response doesn't include our local user's join event,
	// so we need to merge it into the new state snapshot. Without this, the local user's
	// join would be lost when we replace the state, breaking membership table updates.
	roomState := state.NewStateResolution(updater, roomInfo, r.Queryer)
	var oldStateEntries []types.StateEntry
	oldStateEntries, err = roomState.LoadStateAtSnapshot(ctx, oldStateNID)
	if err != nil {
		return false, nil, fmt.Errorf("roomState.LoadStateAtSnapshot: %w", err)
	}

	// The remote /state response reflects the room *before* our join, so for our
	// own (local) users it can only carry a stale membership (e.g. our pre-join
	// invite). Resolve which overlapping old-state member events target local
	// users; those must override the remote state rather than be regressed by it.
	overlapping := overlappingOldMembers(stateEntries, oldStateEntries)
	localOldMemberNIDs := make(map[types.EventNID]bool)
	if len(overlapping) > 0 {
		nids := make([]types.EventNID, len(overlapping))
		for i := range overlapping {
			nids[i] = overlapping[i].EventNID
		}
		var events []types.Event
		events, err = r.DB.Events(ctx, roomInfo.RoomVersion, nids)
		if err != nil {
			return false, nil, fmt.Errorf("r.DB.Events: %w", err)
		}
		for i := range overlapping {
			if ev, ok := helpers.EventMap(events).Lookup(overlapping[i].EventNID); ok && r.isLocalTarget(ctx, ev) {
				localOldMemberNIDs[overlapping[i].EventNID] = true
			}
		}
	}

	var keptLocalMembers int
	stateEntries, keptLocalMembers = reconcileResyncMembers(stateEntries, oldStateEntries, localOldMemberNIDs)
	if keptLocalMembers > 0 {
		logger.WithField("preserved_local_members", keptLocalMembers).
			Info("Preserved local member events in new state snapshot")
	}

	// Deduplicate again after adding local member events
	stateEntries = types.DeduplicateStateEntries(stateEntries)

	// Create a new state snapshot from the merged state entries
	var newStateNID types.StateSnapshotNID
	newStateNID, err = updater.AddState(ctx, roomInfo.RoomNID, nil, stateEntries)
	if err != nil {
		return false, nil, fmt.Errorf("updater.AddState: %w", err)
	}

	logger.WithField("new_state_nid", newStateNID).Debug("Created new state snapshot")

	// Calculate the state delta between old and new snapshots
	// Note: roomState was already created above for LoadStateAtSnapshot
	var removed, added []types.StateEntry
	removed, added, err = roomState.DifferenceBetweeenStateSnapshots(ctx, oldStateNID, newStateNID)
	if err != nil {
		return false, nil, fmt.Errorf("roomState.DifferenceBetweeenStateSnapshots: %w", err)
	}

	// Debug: Count EventTypeNIDs in added slice
	eventTypeNIDCounts := make(map[types.EventTypeNID]int)
	memberCount := 0
	for _, entry := range added {
		eventTypeNIDCounts[entry.EventTypeNID]++
		if entry.EventTypeNID == types.MRoomMemberNID {
			memberCount++
		}
	}

	logger.WithFields(logrus.Fields{
		"removed":               len(removed),
		"added":                 len(added),
		"added_member_events":   memberCount,
		"event_type_nid_counts": eventTypeNIDCounts,
		"MRoomMemberNID":        types.MRoomMemberNID,
	}).Debug("Calculated state delta with EventTypeNID breakdown")

	// MSC3706 Fix: Ensure all membership events in the new state have corresponding
	// membership rows, not just those in the state delta. This handles the case where
	// a membership event (e.g., the local user's join) was stored during partial state
	// join but the membership table was never updated because the event was treated
	// as an outlier.
	//
	// We need to process ALL membership events from the fetched state, not just
	// those that differ from the old state snapshot.
	addedMemberKeys := make(map[types.EventStateKeyNID]bool)
	for _, entry := range added {
		if entry.EventTypeNID == types.MRoomMemberNID {
			addedMemberKeys[entry.EventStateKeyNID] = true
		}
	}

	// Add membership events from stateEntries that aren't already in added
	membershipEntriesAdded := 0
	for _, entry := range stateEntries {
		if entry.EventTypeNID == types.MRoomMemberNID && !addedMemberKeys[entry.EventStateKeyNID] {
			added = append(added, entry)
			addedMemberKeys[entry.EventStateKeyNID] = true
			membershipEntriesAdded++
		}
	}

	if membershipEntriesAdded > 0 {
		logger.WithField("membership_entries_added", membershipEntriesAdded).
			Info("Added membership events from full state to ensure membership rows exist")
	}

	// Update memberships based on the state delta plus any missing membership events
	var outputEvents []api.OutputEvent
	if len(removed) > 0 || len(added) > 0 {
		// Count membership changes that will be processed
		memberChanges := 0
		for _, entry := range added {
			if entry.EventTypeNID == types.MRoomMemberNID {
				memberChanges++
			}
		}
		for _, entry := range removed {
			if entry.EventTypeNID == types.MRoomMemberNID {
				memberChanges++
			}
		}
		logger.WithField("member_changes_to_process", memberChanges).Debug("About to update memberships")

		var resyncLeft bool
		outputEvents, resyncLeft, err = r.updateMemberships(ctx, updater, removed, added)
		if err != nil {
			return false, nil, fmt.Errorf("r.updateMemberships: %w", err)
		}
		if resyncLeft {
			localLeftJoin = true
		}
		logger.WithFields(logrus.Fields{
			"output_events":  len(outputEvents),
			"member_changes": memberChanges,
		}).Debug("Updated memberships (output_events are for retired invites only)")
	}

	// Update the current state snapshot in the room
	// We need to use SetLatestEvents, but we want to keep the latest events unchanged
	// Just update the state snapshot NID
	latestEvents := updater.LatestEvents()
	if len(latestEvents) == 0 {
		// This shouldn't happen for a room with events, but handle gracefully
		logger.Warn("No latest events found for room, skipping state snapshot update")
		succeeded = true
		return localLeftJoin, roomInfo, nil
	}

	// Get the last event NID that was sent
	lastEventNID := latestEvents[0].EventNID
	for _, latest := range latestEvents {
		if latest.EventNID > lastEventNID {
			lastEventNID = latest.EventNID
		}
	}

	// Update the latest events with the new state snapshot
	if err = updater.SetLatestEvents(roomInfo.RoomNID, latestEvents, lastEventNID, newStateNID); err != nil {
		return false, nil, fmt.Errorf("updater.SetLatestEvents: %w", err)
	}

	// The resync only replaces the room's current state. The latest events still
	// carry the partial state they were stored with, and every later event's
	// state is calculated from them, so fill in what they are missing.
	// Otherwise each recalculation drops resync-only members again.
	var repaired int
	repaired, err = repairExtremityState(ctx, updater, &roomState, roomInfo.RoomNID, latestEvents, stateEntries)
	if err != nil {
		return false, nil, fmt.Errorf("repairExtremityState: %w", err)
	}
	if repaired > 0 {
		logger.WithField("repaired_extremities", repaired).Info("Filled in partial state at forward extremities")
	}

	// MSC3706 State Epoch Fix: Record the state snapshot NID after resync completes.
	// This marks the current state as the "authoritative" state from the partial state resync.
	// When processing events later, we use this to detect and suppress state regressions
	// caused by out-of-order events that reference older positions in the DAG.
	if err = updater.UpdateResyncStateNID(roomInfo.RoomNID, newStateNID); err != nil {
		return false, nil, fmt.Errorf("updater.UpdateResyncStateNID: %w", err)
	}

	logger.WithField("resync_state_nid", newStateNID).Debug("Recorded resync state NID to prevent state regressions")

	// Emit output events to notify downstream components about membership changes
	if len(outputEvents) > 0 {
		if err = r.OutputProducer.ProduceRoomEvents(roomID, outputEvents); err != nil {
			return false, nil, fmt.Errorf("r.OutputProducer.ProduceRoomEvents: %w", err)
		}
		logger.WithField("output_events", len(outputEvents)).Debug("Produced output events for membership changes")
	}

	succeeded = true

	logger.WithFields(logrus.Fields{
		"old_state_nid": oldStateNID,
		"new_state_nid": newStateNID,
		"removed":       len(removed),
		"added":         len(added),
	}).Info("Successfully updated current state after partial state resync")

	return localLeftJoin, roomInfo, nil
}

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

// overlappingOldMembers returns the member events from oldState whose state key
// is also present as a *different* member event in remoteState. These are the
// candidates whose target locality must be resolved before deciding whether the
// remote /state's (older) version may replace ours.
func overlappingOldMembers(remoteState, oldState []types.StateEntry) []types.StateEntry {
	remoteMembers := make(map[types.StateKeyTuple]types.EventNID)
	for _, e := range remoteState {
		if e.EventTypeNID == types.MRoomMemberNID {
			remoteMembers[e.StateKeyTuple] = e.EventNID
		}
	}
	var out []types.StateEntry
	for _, e := range oldState {
		if e.EventTypeNID != types.MRoomMemberNID {
			continue
		}
		if nid, ok := remoteMembers[e.StateKeyTuple]; ok && nid != e.EventNID {
			out = append(out, e)
		}
	}
	return out
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

// reconcileResyncMembers merges membership state for a partial-state resync.
// RemoteState is the state fetched from the remote server (which reflects the
// room *before* our join); oldState is our current state. It returns remoteState
// adjusted so that:
//   - member events present in oldState but absent from remoteState are kept, and
//   - member events present in both but differing are taken from oldState when the
//     old event's NID is in localOldMemberNIDs (a local user, whose membership the
//     remote /state must not be allowed to regress, e.g. join -> stale invite).
//
// The second return value is the number of member events taken from oldState.
func reconcileResyncMembers(
	remoteState, oldState []types.StateEntry,
	localOldMemberNIDs map[types.EventNID]bool,
) ([]types.StateEntry, int) {
	memberPos := make(map[types.StateKeyTuple]int)
	for i, e := range remoteState {
		if e.EventTypeNID == types.MRoomMemberNID {
			memberPos[e.StateKeyTuple] = i
		}
	}

	kept := 0
	for _, e := range oldState {
		if e.EventTypeNID != types.MRoomMemberNID {
			continue
		}
		pos, ok := memberPos[e.StateKeyTuple]
		if !ok {
			// Absent from the remote state - keep ours.
			remoteState = append(remoteState, e)
			memberPos[e.StateKeyTuple] = len(remoteState) - 1
			kept++
			continue
		}
		if remoteState[pos].EventNID != e.EventNID && localOldMemberNIDs[e.EventNID] {
			remoteState[pos] = e
			kept++
		}
	}
	return remoteState, kept
}
