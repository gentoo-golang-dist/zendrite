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

// TestSplitCandidateRoomsByStatus_PreviouslyRoom_AlwaysIncludedNoQuery covers
// the room-selection side of the same gap pattern as getTimelineEvents: a
// PREVIOUSLY room must be included unconditionally, not via the LIVE batch
// query, or it never re-enters roomsToPopulate on its own.
func TestSplitCandidateRoomsByStatus_PreviouslyRoom_AlwaysIncludedNoQuery(t *testing.T) {
	t.Parallel()

	roomStates := map[string]types.RoomStreamState{
		"!gappy:localhost": {
			Status:    types.HaveSentRoomPreviously,
			LastToken: &types.StreamingToken{PDUPosition: 50},
		},
	}

	liveRoomIDs, alwaysIncludeReasons := splitCandidateRoomsByStatus(roomStates)

	assert.Empty(t, liveRoomIDs, "a PREVIOUSLY room must not need the LIVE batch query")
	assert.Equal(t, "status_previously", alwaysIncludeReasons["!gappy:localhost"])
}

// TestSplitCandidateRoomsByStatus_NeverRoom_AlwaysIncludedNoQuery covers the
// other unconditional-include status: a room never sent on this connection
// at all must also bypass the batch query.
func TestSplitCandidateRoomsByStatus_NeverRoom_AlwaysIncludedNoQuery(t *testing.T) {
	t.Parallel()

	roomStates := map[string]types.RoomStreamState{
		"!new:localhost": {
			Status:    types.HaveSentRoomNever,
			LastToken: nil,
		},
	}

	liveRoomIDs, alwaysIncludeReasons := splitCandidateRoomsByStatus(roomStates)

	assert.Empty(t, liveRoomIDs)
	assert.Equal(t, "status_never", alwaysIncludeReasons["!new:localhost"])
}

// TestSplitCandidateRoomsByStatus_MixedRooms_OnlyLiveNeedsQuery ensures LIVE
// rooms are the only ones routed to the batch DB query, and that they don't
// end up in the unconditional-include set - a single connection-wide floor
// is only valid for rooms that are actually caught up.
func TestSplitCandidateRoomsByStatus_MixedRooms_OnlyLiveNeedsQuery(t *testing.T) {
	t.Parallel()

	roomStates := map[string]types.RoomStreamState{
		"!live-a:localhost": {
			Status:    types.HaveSentRoomLive,
			LastToken: &types.StreamingToken{PDUPosition: 95},
		},
		"!live-b:localhost": {
			Status:    types.HaveSentRoomLive,
			LastToken: &types.StreamingToken{PDUPosition: 100},
		},
		"!gappy:localhost": {
			Status:    types.HaveSentRoomPreviously,
			LastToken: &types.StreamingToken{PDUPosition: 40},
		},
		"!new:localhost": {
			Status:    types.HaveSentRoomNever,
			LastToken: nil,
		},
	}

	liveRoomIDs, alwaysIncludeReasons := splitCandidateRoomsByStatus(roomStates)

	assert.ElementsMatch(t, []string{"!live-a:localhost", "!live-b:localhost"}, liveRoomIDs)
	assert.Len(t, alwaysIncludeReasons, 2)
	assert.Equal(t, "status_previously", alwaysIncludeReasons["!gappy:localhost"])
	assert.Equal(t, "status_never", alwaysIncludeReasons["!new:localhost"])
}
