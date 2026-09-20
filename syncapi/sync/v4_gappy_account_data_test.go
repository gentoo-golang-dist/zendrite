// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sync

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/syncapi/types"
)

// TestProcessAccountDataExtension_RoomNotInCurrentResponse_StillDeliversAccountData
// is a currently-FAILING regression test (intentionally, pending the fix
// discussed with the user) documenting a second instance of the same bug class
// fixed in v4_gappy_timeline_test.go, this time in the account_data extension.
//
// processAccountDataExtension queries ALL account data changes for the user in
// [from, to] via GetAccountDataInRange - a connection-wide, room-agnostic range
// - and then filters the result down to relevantRoomIDs, which is only the set
// of rooms that happen to appear in THIS response's lists/subscriptions. Any
// room-scoped account data (m.fully_read markers, room tags, ...) for a room
// that isn't touched by this particular response's list ops gets silently
// dropped, even though the query already fetched it - and the account data
// position still advances regardless, so it is never delivered on a later
// sync either.
//
// Synapse's reference implementation avoids this by tracking a PREVIOUSLY/LIVE
// status *per room, specific to the account_data extension*
// (previous_connection_state.account_data.have_sent_room), and for PREVIOUSLY
// rooms queries that room's own last-sent token individually
// (synapse/handlers/sliding_sync/extensions.py:515-526), instead of relying on
// "was this room touched in the current response".
//
// zendrite has no such per-room tracking for account_data - unlike the
// timeline, which already had (and, after the fix, has again) a per-room
// LastToken via SlidingSyncConnectionStream. The DB schema's `stream` column
// on that table already anticipates an "account_data" stream value in its
// doc comment, but nothing ever writes/reads it.
func TestProcessAccountDataExtension_RoomNotInCurrentResponse_StillDeliversAccountData(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	userID := "@alice:localhost"

	const (
		roomInResponse    = "!roomA:localhost" // has a list op / subscription this response
		roomNotInResponse = "!roomB:localhost" // quiet this response - no list op, no subscription
	)

	fullyReadA, err := json.Marshal(map[string]string{"event_id": "$readA:localhost"})
	require.NoError(t, err)
	fullyReadB, err := json.Marshal(map[string]string{"event_id": "$readB:localhost"})
	require.NoError(t, err)
	pushRules, err := json.Marshal(map[string]any{"global": map[string]any{}})
	require.NoError(t, err)

	snapshot := newMockSnapshot()
	// The range query legitimately reports changes for BOTH rooms - this is
	// what the real DB query does, since it has no concept of "which rooms are
	// in the current response".
	snapshot.SetAccountDataInRange(map[string][]string{
		"":                []string{"m.push_rules"},
		roomInResponse:    []string{"m.fully_read"},
		roomNotInResponse: []string{"m.fully_read"},
	}, 200)

	rp := &RequestPool{
		userAPI: &mockUserAPI{
			globalAccountData: map[string]json.RawMessage{
				"m.push_rules": pushRules,
			},
			roomAccountData: map[string]map[string]json.RawMessage{
				roomInResponse:    {"m.fully_read": fullyReadA},
				roomNotInResponse: {"m.fully_read": fullyReadB},
			},
		},
	}

	fromPos := &types.StreamingToken{AccountDataPosition: 100}
	toPos := types.StreamingToken{AccountDataPosition: 200}

	// roomNotInResponse deliberately appears in neither lists nor subscriptions -
	// exactly like a room the client isn't currently touching because nothing
	// else about it changed this cycle.
	actualLists := map[string]types.SlidingList{}
	actualRoomSubscriptions := map[string]bool{roomInResponse: true}

	resp, _, err := rp.processAccountDataExtension(
		ctx, snapshot, userID,
		nil, nil, // requestedLists, requestedRooms: default wildcard
		fromPos, toPos,
		actualLists, actualRoomSubscriptions,
	)
	require.NoError(t, err)

	require.Contains(t, resp.Rooms, roomInResponse,
		"account data for a room that IS in the current response must be delivered")

	// This is the bug: roomNotInResponse's account data was fetched by the
	// range query (it's a real change in [from, to]) but gets silently
	// dropped because the room isn't in this response's lists/subscriptions.
	// Expected to FAIL until per-room account_data tracking is implemented.
	assert.Contains(t, resp.Rooms, roomNotInResponse,
		"account data for a room NOT in the current response must still be delivered eventually, not dropped forever")
}
