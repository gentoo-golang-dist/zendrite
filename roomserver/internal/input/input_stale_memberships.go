// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"context"
	"fmt"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/storage/shared"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// repairStaleMemberships adds to the room's current state the memberships
// stored as outliers that follow the membership the current state holds for
// their user. Such a membership, a leave, kick, ban or rejoin that arrived
// only as an auth event of a later event, never entered the state, so the
// room kept treating the user as before it.
//
// A membership event cites the membership its user had before it among its
// auth events, so the memberships that follow the current one form a chain
// back to it. For each user the latest membership of that chain is taken; if
// two of them are equally far from the current one, the user is left alone.
// The membership must also pass auth against the current state, as a new event
// must before it enters it, and then replaces the current one in the current
// state and in the state before each forward extremity that holds it. State
// resolution is not asked: it orders conflicting events by the power levels
// they cite and their timestamps, and a leave citing an older power levels
// event than the join it follows would lose to it. A rejoin that only passed
// with an authorization or state the room no longer holds stays out.
//
// It adds the state keys of the memberships it added to memberKeys, and
// returns how many it added and the output events announcing them.
func (r *Inputer) repairStaleMemberships(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution,
	roomInfo *types.RoomInfo, memberKeys map[types.EventStateKeyNID]struct{},
) (int, []api.OutputEvent, error) {
	outlierNIDs, err := updater.AcceptedOutlierMembershipNIDs()
	if err != nil {
		return 0, nil, fmt.Errorf("updater.AcceptedOutlierMembershipNIDs: %w", err)
	}
	if len(outlierNIDs) == 0 {
		return 0, nil, nil
	}
	current, err := roomState.LoadStateAtSnapshot(ctx, updater.CurrentStateSnapshotNID())
	if err != nil {
		return 0, nil, fmt.Errorf("roomState.LoadStateAtSnapshot: %w", err)
	}
	currentByKey := map[types.EventStateKeyNID]types.EventNID{}
	for _, entry := range current {
		if entry.EventTypeNID == types.MRoomMemberNID {
			currentByKey[entry.EventStateKeyNID] = entry.EventNID
		}
	}
	outlierEntries, err := updater.StateEntriesForEventNIDs(ctx, outlierNIDs)
	if err != nil {
		return 0, nil, fmt.Errorf("updater.StateEntriesForEventNIDs: %w", err)
	}
	byKey := map[types.EventStateKeyNID][]types.StateEntry{}
	needed := map[types.EventNID]struct{}{}
	for _, entry := range outlierEntries {
		currentNID, ok := currentByKey[entry.EventStateKeyNID]
		if !ok || currentNID == entry.EventNID {
			continue
		}
		byKey[entry.EventStateKeyNID] = append(byKey[entry.EventStateKeyNID], entry)
		needed[entry.EventNID] = struct{}{}
		needed[currentNID] = struct{}{}
	}
	if len(byKey) == 0 {
		return 0, nil, nil
	}
	neededNIDs := make([]types.EventNID, 0, len(needed))
	for nid := range needed {
		neededNIDs = append(neededNIDs, nid)
	}
	events, err := updater.Events(ctx, roomInfo.RoomVersion, neededNIDs)
	if err != nil {
		return 0, nil, fmt.Errorf("updater.Events: %w", err)
	}
	eventsByNID := make(map[types.EventNID]types.Event, len(events))
	for _, ev := range events {
		eventsByNID[ev.EventNID] = ev
	}

	var heads []types.StateEntry
	for key, entries := range byKey {
		currentEvent, ok := eventsByNID[currentByKey[key]]
		if !ok {
			continue
		}
		chain := make(map[string]types.Event, len(entries))
		for _, entry := range entries {
			if ev, found := eventsByNID[entry.EventNID]; found {
				chain[ev.EventID()] = ev
			}
		}
		var head types.StateEntry
		headSteps, tied := 0, false
		for _, entry := range entries {
			ev, found := eventsByNID[entry.EventNID]
			if !found {
				continue
			}
			steps := stepsToMembership(ev, currentEvent.EventID(), chain)
			switch {
			case steps > headSteps:
				head, headSteps, tied = entry, steps, false
			case steps > 0 && steps == headSteps:
				tied = true
			}
		}
		if headSteps > 0 && !tied {
			heads = append(heads, head)
		}
	}
	if len(heads) == 0 {
		return 0, nil, nil
	}
	if heads, err = r.withoutSoftFailed(ctx, updater, roomState, roomInfo, heads); err != nil {
		return 0, nil, err
	}
	if len(heads) == 0 {
		return 0, nil, nil
	}

	replace := make(map[types.EventNID]types.StateEntry, len(heads))
	for _, head := range heads {
		replace[currentByKey[head.EventStateKeyNID]] = head
	}
	stateBefore := updater.CurrentStateSnapshotNID()
	if err = replaceMemberships(ctx, updater, roomState, roomInfo.RoomNID, replace); err != nil {
		return 0, nil, err
	}
	for _, head := range heads {
		memberKeys[head.EventStateKeyNID] = struct{}{}
	}
	updates, err := acceptedMembershipOutputs(ctx, updater, roomState, roomInfo, stateBefore, heads)
	if err != nil {
		return 0, nil, err
	}
	return len(heads), updates, nil
}

// replaceMemberships replaces the given membership events by the entries
// they map to in the room's current state and in the state before each of
// its forward extremities.
func replaceMemberships(
	ctx context.Context, updater *shared.RoomUpdater, roomState *state.StateResolution,
	roomNID types.RoomNID, replace map[types.EventNID]types.StateEntry,
) error {
	done := map[types.StateSnapshotNID]types.StateSnapshotNID{}
	replaced := func(stateNID types.StateSnapshotNID) (types.StateSnapshotNID, error) {
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
		changed := false
		for i, entry := range entries {
			if next, ok := replace[entry.EventNID]; ok && next.StateKeyTuple == entry.StateKeyTuple {
				entries[i] = next
				changed = true
			}
		}
		newNID := types.StateSnapshotNID(0)
		if changed {
			if newNID, err = updater.AddState(ctx, roomNID, nil, entries); err != nil {
				return 0, fmt.Errorf("updater.AddState: %w", err)
			}
		}
		done[stateNID] = newNID
		return newNID, nil
	}

	newCurrent, err := replaced(updater.CurrentStateSnapshotNID())
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
		newBefore, rerr := replaced(latest.BeforeStateSnapshotNID)
		if rerr != nil {
			return rerr
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

// stepsToMembership returns how many memberships of the same user lead back
// from the given membership event to the one with the given ID, following
// the membership each cites among its auth events through the given chain,
// or 0 if they do not lead to it.
func stepsToMembership(ev types.Event, targetID string, chain map[string]types.Event) int {
	for steps := 1; steps <= maxJoinRun; steps++ {
		var prior types.Event
		found := false
		for _, authID := range ev.AuthEventIDs() {
			if authID == targetID {
				return steps
			}
			if p, ok := chain[authID]; ok && p.Type() == spec.MRoomMember {
				prior, found = p, true
			}
		}
		if !found {
			return 0
		}
		ev = prior
	}
	return 0
}
