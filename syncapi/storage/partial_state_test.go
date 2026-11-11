// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package storage_test

import (
	"math"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
)

// TestPartialStateResyncStorage checks what the sync API stores for a room
// whose partial-state resync finished (MSC3902): the resync is recorded at a
// position of the PDU stream, so that an incremental sync finds it in its
// range, and an event the full state rejects is no longer served.
func TestPartialStateResyncStorage(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		room := test.NewRoom(t, alice)
		db, closeDB := MustCreateDatabase(t, dbType)
		defer closeDB()

		rejected := room.CreateAndInsert(t, alice, "m.room.test", map[string]any{"body": "rejected"}, test.WithStateKey(""))
		positions := MustWriteEvents(t, db, room.Events())
		before := positions[len(positions)-1]

		pos, err := db.InsertUnPartialStatedRoom(ctx, room.ID, alice.ID)
		require.NoError(t, err)
		assert.Greater(t, pos, before, "the resync takes a position of the PDU stream")

		require.NoError(t, db.DeleteRejectedEvent(ctx, rejected.EventID()))

		snapshot, err := db.NewDatabaseSnapshot(ctx)
		require.NoError(t, err)
		defer snapshot.Rollback() //nolint:errcheck

		roomIDs, err := snapshot.UnPartialStatedRoomsInRange(ctx, alice.ID, types.Range{From: before, To: pos})
		require.NoError(t, err)
		assert.Equal(t, []string{room.ID}, roomIDs)
		roomIDs, err = snapshot.UnPartialStatedRoomsInRange(ctx, alice.ID, types.Range{From: pos, To: pos + 10})
		require.NoError(t, err)
		assert.Empty(t, roomIDs)

		events, err := snapshot.Events(ctx, []string{rejected.EventID()})
		require.NoError(t, err)
		assert.Empty(t, events, "the rejected event is no longer served")
		filter := synctypes.DefaultStateFilter()
		state, err := snapshot.CurrentState(ctx, room.ID, &filter, nil)
		require.NoError(t, err)
		for _, ev := range state {
			assert.NotEqual(t, rejected.EventID(), ev.EventID())
		}
		var sawCreate bool
		for _, ev := range state {
			sawCreate = sawCreate || ev.Type() == spec.MRoomCreate
		}
		assert.True(t, sawCreate, "the rest of the state is kept")
	})
}

// TestPartialStateResyncKeepsRejoin checks that the full state written after a
// resync keeps a rejoin current over the leave before it.
func TestPartialStateResyncKeepsRejoin(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		db, closeDB := MustCreateDatabase(t, dbType)
		defer closeDB()

		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(bob.ID))
		MustWriteEvents(t, db, room.Events())
		// Bob rejoins with partial state; the resync writes the full state.
		rejoin := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		_, err := db.PopulateRoomStateAfterResync(ctx, room.CurrentState())
		require.NoError(t, err)
		require.Contains(t, room.CurrentState(), rejoin)

		snapshot, err := db.NewDatabaseSnapshot(ctx)
		require.NoError(t, err)
		defer snapshot.Rollback() //nolint:errcheck
		membership, _, err := snapshot.SelectMembershipForUser(ctx, room.ID, bob.ID, math.MaxInt64)
		require.NoError(t, err)
		assert.Equal(t, spec.Join, membership)
	})
}
