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

// TestProcessAccountDataExtension_RoomReenteringAfterGap_StillDeliversAccountData
// guards against account_data queried from the connection-wide fromPos
// instead of a room's own last-sent token (see accountDataFloorForRoom):
// a room excluded from prior responses would have changes skipped once it
// re-enters view.
func TestProcessAccountDataExtension_RoomReenteringAfterGap_StillDeliversAccountData(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	userID := "@alice:localhost"

	const (
		roomSteady = "!roomA:localhost" // continuously in view, no gap
		roomGappy  = "!roomB:localhost" // was excluded for a while, now back in view
	)

	fullyReadSteady, err := json.Marshal(map[string]string{"event_id": "$readA:localhost"})
	require.NoError(t, err)
	fullyReadGap, err := json.Marshal(map[string]string{"event_id": "$readB-missed-while-excluded:localhost"})
	require.NoError(t, err)

	snapshot := newMockSnapshot()
	// roomGappy's change at 60 is inside the gap between its own last-sent
	// position (50) and the connection's since token (90) - see below.
	snapshot.SetAccountDataChange(roomSteady, "m.fully_read", 95)
	snapshot.SetAccountDataChange(roomGappy, "m.fully_read", 60)

	db := newMockStreamStateDB()
	rp := &RequestPool{
		db: db,
		userAPI: &mockUserAPI{
			roomAccountData: map[string]map[string]json.RawMessage{
				roomSteady: {"m.fully_read": fullyReadSteady},
				roomGappy:  {"m.fully_read": fullyReadGap},
			},
		},
	}

	fromPos := &types.StreamingToken{AccountDataPosition: 90}
	toPos := types.StreamingToken{AccountDataPosition: 200}

	connState := &V4ConnectionState{
		ConnectionKey: 1,
		// A real response position: gap recovery is only attempted when the
		// result can be recorded, so pinning this to 0 would silently disable
		// the behavior under test.
		ConnectionPosition: 42,
		PreviousStreamStates: map[string]map[string]*types.SlidingSyncStreamState{
			roomSteady: {
				accountDataStream: {LastToken: (&types.StreamingToken{AccountDataPosition: 90}).String()},
			},
			roomGappy: {
				accountDataStream: {LastToken: (&types.StreamingToken{AccountDataPosition: 50}).String()},
			},
		},
	}

	// Both rooms are relevant to THIS response - roomGappy has just re-entered
	// view (e.g. the client re-subscribed or it re-entered a list range).
	actualLists := map[string]types.SlidingList{}
	actualRoomSubscriptions := map[string]bool{roomSteady: true, roomGappy: true}

	resp, _, err := rp.processAccountDataExtension(
		ctx, snapshot, connState, userID,
		nil, nil, // requestedLists, requestedRooms: default wildcard
		fromPos, toPos,
		actualLists, actualRoomSubscriptions,
	)
	require.NoError(t, err)

	assert.Contains(t, resp.Rooms, roomSteady,
		"a continuously-live room's account data must still be delivered")
	assert.Contains(t, resp.Rooms, roomGappy,
		"account data missed while a room was excluded from responses must be delivered once it's relevant again, not dropped forever")

	// Both rooms must record how far they were actually served, or the gap
	// reopens on the next response.
	for _, roomID := range []string{roomSteady, roomGappy} {
		key := streamKey(roomID, accountDataStream)
		require.Contains(t, db.writes, key, "every relevant room records its delivered position")
		assert.Equal(t, int64(42), db.writes[key].position)
	}
	assert.True(t, connState.AccountDataWritten[roomGappy],
		"a freshly written room must not then be copied forward at its stale position")
}

// TestProcessAccountDataExtension_DeletedGlobalAccountData checks that a
// global type that changed in range but no longer exists (e.g. the MSC3890
// settings of a deleted device) is sent with empty content on incremental
// responses and left out of initial ones, as Synapse does.
func TestProcessAccountDataExtension_DeletedGlobalAccountData(t *testing.T) {
	t.Parallel()
	const deleted = "org.matrix.msc3890.local_notification_settings.DEVICE"

	snapshot := newMockSnapshot()
	snapshot.SetAccountDataChange("", deleted, 95)
	rp := &RequestPool{db: newMockStreamStateDB(), userAPI: &mockUserAPI{}}
	toPos := types.StreamingToken{AccountDataPosition: 200}

	resp, _, err := rp.processAccountDataExtension(
		context.Background(), snapshot, nil, "@alice:localhost",
		nil, nil, &types.StreamingToken{AccountDataPosition: 90}, toPos, nil, nil,
	)
	require.NoError(t, err)
	require.Len(t, resp.Global, 1)
	assert.Equal(t, deleted, resp.Global[0].Type)
	assert.JSONEq(t, `{}`, string(resp.Global[0].Content))

	resp, _, err = rp.processAccountDataExtension(
		context.Background(), snapshot, nil, "@alice:localhost",
		nil, nil, nil, toPos, nil, nil,
	)
	require.NoError(t, err)
	assert.Empty(t, resp.Global, "initial responses leave deleted account data out")
}

// TestProcessAccountDataExtension_GlobalOnFirstEnable checks that the first
// response of a connection to carry the account_data extension sends all
// global account data, not only what changed since pos (MSC4548), and that
// later responses go back to changes only.
func TestProcessAccountDataExtension_GlobalOnFirstEnable(t *testing.T) {
	t.Parallel()
	const deleted = "org.matrix.msc3890.local_notification_settings.DEVICE"

	snapshot := newMockSnapshot()
	const deletedSincePos = "org.matrix.msc3890.local_notification_settings.OTHER"
	snapshot.SetAccountDataChange("", "m.direct", 10)
	snapshot.SetAccountDataChange("", deleted, 20)
	snapshot.SetAccountDataChange("", deletedSincePos, 95)
	db := newMockStreamStateDB()
	rp := &RequestPool{db: db, userAPI: &mockUserAPI{
		globalAccountData: map[string]json.RawMessage{"m.direct": json.RawMessage(`{}`)},
	}}
	fromPos := &types.StreamingToken{AccountDataPosition: 90}
	toPos := types.StreamingToken{AccountDataPosition: 200}

	// The extension was off for the responses so far: the connection has no
	// global account data state.
	connState := &V4ConnectionState{ConnectionKey: 1, ConnectionPosition: 42, PreviousStreamStates: map[string]map[string]*types.SlidingSyncStreamState{}}
	resp, _, err := rp.processAccountDataExtension(context.Background(), snapshot, connState, "@alice:localhost", nil, nil, fromPos, toPos, nil, nil)
	require.NoError(t, err)
	sent := map[string]string{}
	for _, ev := range resp.Global {
		sent[ev.Type] = string(ev.Content)
	}
	assert.Equal(t, map[string]string{"m.direct": "{}", deletedSincePos: "{}"}, sent,
		"all global account data, without what was deleted before pos, but with what was deleted since")
	key := streamKey(globalAccountDataScope, accountDataStream)
	require.Contains(t, db.writes, key, "the connection records that it sent global account data")

	// Once sent, only changes after pos follow: here the deletion.
	connState = &V4ConnectionState{ConnectionKey: 1, ConnectionPosition: 43, PreviousStreamStates: map[string]map[string]*types.SlidingSyncStreamState{
		globalAccountDataScope: {accountDataStream: {LastToken: (&types.StreamingToken{AccountDataPosition: 90}).String()}},
	}}
	resp, _, err = rp.processAccountDataExtension(context.Background(), snapshot, connState, "@alice:localhost", nil, nil, fromPos, toPos, nil, nil)
	require.NoError(t, err)
	require.Len(t, resp.Global, 1)
	assert.Equal(t, deletedSincePos, resp.Global[0].Type)
}

// TestProcessAccountDataExtension_DeletedRoomAccountData checks that room
// account data deleted with MSC3391 is sent with empty content on
// incremental responses, and left out for a room new to the connection,
// whose account data is all sent.
func TestProcessAccountDataExtension_DeletedRoomAccountData(t *testing.T) {
	t.Parallel()
	const (
		roomID  = "!room:localhost"
		deleted = "org.example.deleted"
	)

	snapshot := newMockSnapshot()
	snapshot.SetAccountDataChange(roomID, deleted, 95)
	rp := &RequestPool{db: newMockStreamStateDB(), userAPI: &mockUserAPI{}}
	fromPos := &types.StreamingToken{AccountDataPosition: 90}
	toPos := types.StreamingToken{AccountDataPosition: 200}
	subscriptions := map[string]bool{roomID: true}

	resp, _, err := rp.processAccountDataExtension(
		context.Background(), snapshot, nil, "@alice:localhost",
		nil, nil, fromPos, toPos, nil, subscriptions,
	)
	require.NoError(t, err)
	require.Len(t, resp.Rooms[roomID], 1)
	assert.Equal(t, deleted, resp.Rooms[roomID][0].Type)
	assert.JSONEq(t, `{}`, string(resp.Rooms[roomID][0].Content))

	// The room is new to this connection: its account data is read from the
	// start, without what was deleted.
	connState := &V4ConnectionState{ConnectionKey: 1, ConnectionPosition: 42, PreviousStreamStates: map[string]map[string]*types.SlidingSyncStreamState{}}
	resp, _, err = rp.processAccountDataExtension(
		context.Background(), snapshot, connState, "@alice:localhost",
		nil, nil, fromPos, toPos, nil, subscriptions,
	)
	require.NoError(t, err)
	assert.Empty(t, resp.Rooms[roomID], "a room new to the connection leaves deleted account data out")
}
