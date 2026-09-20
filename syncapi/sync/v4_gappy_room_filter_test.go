// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"codefloe.com/pat-s/zendrite/syncapi/types"
)

// TestComputePDUQueryFloor_LaggedPreviouslyRoom_WidensFloor is the room-selection
// counterpart of the timeline/account_data/required_state gap fixes: a room
// that fell behind the connection-wide since token (status PREVIOUSLY, e.g.
// excluded from one or more prior responses while unrelated activity kept
// advancing the connection's own position) must still be considered
// "possibly changed" by the batch RoomsWithEventsSince pre-filter in
// HandleV4Sync - otherwise it never re-enters roomsToPopulate until some
// other event happens to push it past the connection-wide since on its own
// (e.g. a "nudge" message), even though it has real pending events the
// client never received.
func TestComputePDUQueryFloor_LaggedPreviouslyRoom_WidensFloor(t *testing.T) {
	t.Parallel()

	connectionFloor := types.StreamPosition(90)
	roomStates := map[string]types.RoomStreamState{
		"!gappy:localhost": {
			Status:    types.HaveSentRoomPreviously,
			LastToken: &types.StreamingToken{PDUPosition: 50},
		},
	}

	floor := computePDUQueryFloor(connectionFloor, roomStates)

	assert.Equal(t, types.StreamPosition(50), floor,
		"a lagged PREVIOUSLY room's own last-sent position must widen the query floor so its pending events aren't invisible to the pre-filter")
}

// TestComputePDUQueryFloor_MultipleRooms_UsesEarliestFloor ensures the batch
// query floor is the minimum across ALL candidate rooms, not just the first
// or the connection-wide value - a single lagged room must not be starved by
// other rooms that are LIVE or freshly caught up.
func TestComputePDUQueryFloor_MultipleRooms_UsesEarliestFloor(t *testing.T) {
	t.Parallel()

	connectionFloor := types.StreamPosition(100)
	roomStates := map[string]types.RoomStreamState{
		"!live:localhost": {
			Status:    types.HaveSentRoomLive,
			LastToken: &types.StreamingToken{PDUPosition: 95},
		},
		"!gappy-a:localhost": {
			Status:    types.HaveSentRoomPreviously,
			LastToken: &types.StreamingToken{PDUPosition: 70},
		},
		"!gappy-b:localhost": {
			Status:    types.HaveSentRoomPreviously,
			LastToken: &types.StreamingToken{PDUPosition: 40},
		},
	}

	floor := computePDUQueryFloor(connectionFloor, roomStates)

	assert.Equal(t, types.StreamPosition(40), floor, "must use the earliest lagged room's position, not just any lagged room's")
}

// TestComputePDUQueryFloor_NoLaggedRooms_KeepsConnectionFloor guards against
// reintroducing an unconditional full-history rescan: rooms that are LIVE
// (fully caught up) or NEVER (brand new, handled separately via the
// status_never path) must not widen the floor - only a genuinely lagged
// PREVIOUSLY room should.
func TestComputePDUQueryFloor_NoLaggedRooms_KeepsConnectionFloor(t *testing.T) {
	t.Parallel()

	connectionFloor := types.StreamPosition(100)
	roomStates := map[string]types.RoomStreamState{
		"!live:localhost": {
			Status:    types.HaveSentRoomLive,
			LastToken: &types.StreamingToken{PDUPosition: 100},
		},
		"!new:localhost": {
			Status:    types.HaveSentRoomNever,
			LastToken: nil,
		},
		"!caught-up:localhost": {
			// PREVIOUSLY but already at the connection floor - no lag, must not widen further.
			Status:    types.HaveSentRoomPreviously,
			LastToken: &types.StreamingToken{PDUPosition: 100},
		},
	}

	floor := computePDUQueryFloor(connectionFloor, roomStates)

	assert.Equal(t, connectionFloor, floor, "with nothing actually lagging, the floor must stay at the connection-wide since position")
}
