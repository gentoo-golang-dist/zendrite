// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package tables_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/storage/postgres"
	"codefloe.com/pat-s/zendrite/roomserver/storage/sqlite3"
	"codefloe.com/pat-s/zendrite/roomserver/storage/tables"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
)

func mustCreatePartialStateEventsTable(t *testing.T, dbType test.DBType) (tables.PartialStateEvents, func()) {
	t.Helper()
	connStr, clearDB := test.PrepareDBConnectionString(t, dbType)
	db, err := sqlutil.Open(&config.DatabaseOptions{
		ConnectionString: config.DataSource(connStr),
	}, sqlutil.NewExclusiveWriter())
	require.NoError(t, err)
	var tab tables.PartialStateEvents
	switch dbType {
	case test.DBTypePostgres:
		require.NoError(t, postgres.CreatePartialStateEventsTable(db))
		tab, err = postgres.PreparePartialStateEventsTable(db)
	case test.DBTypeSQLite:
		require.NoError(t, sqlite3.CreatePartialStateEventsTable(db))
		tab, err = sqlite3.PreparePartialStateEventsTable(db)
	}
	require.NoError(t, err)
	return tab, func() {
		_ = db.Close()
		clearDB()
	}
}

func TestPartialStateEventsTable(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		tab, cleanup := mustCreatePartialStateEventsTable(t, dbType)
		defer cleanup()
		ctx := context.Background()
		room1, room2 := types.RoomNID(1), types.RoomNID(2)

		for _, nid := range []types.EventNID{30, 10, 20} {
			require.NoError(t, tab.InsertPartialStateEvent(ctx, nil, room1, nid))
		}
		require.NoError(t, tab.InsertPartialStateEvent(ctx, nil, room2, 15))
		// Marking an event twice is harmless.
		require.NoError(t, tab.InsertPartialStateEvent(ctx, nil, room1, 10))

		// Oldest first, up to the limit, of the room only.
		nids, err := tab.SelectPartialStateEventsForRoom(ctx, nil, room1, 2)
		require.NoError(t, err)
		assert.Equal(t, []types.EventNID{10, 20}, nids)

		count, err := tab.SelectPartialStateEventCount(ctx, nil, room1)
		require.NoError(t, err)
		assert.Equal(t, int64(3), count)

		partial, err := tab.SelectPartialStateEvents(ctx, nil, []types.EventNID{10, 15, 99})
		require.NoError(t, err)
		assert.ElementsMatch(t, []types.EventNID{10, 15}, partial)
		partial, err = tab.SelectPartialStateEvents(ctx, nil, nil)
		require.NoError(t, err)
		assert.Empty(t, partial)

		require.NoError(t, tab.DeletePartialStateEvent(ctx, nil, 10))
		nids, err = tab.SelectPartialStateEventsForRoom(ctx, nil, room1, 10)
		require.NoError(t, err)
		assert.Equal(t, []types.EventNID{20, 30}, nids)

		require.NoError(t, tab.DeletePartialStateEventsForRoom(ctx, nil, room1))
		count, err = tab.SelectPartialStateEventCount(ctx, nil, room1)
		require.NoError(t, err)
		assert.Zero(t, count)
		count, err = tab.SelectPartialStateEventCount(ctx, nil, room2)
		require.NoError(t, err)
		assert.Equal(t, int64(1), count)
	})
}

// TestPartialStateTableJoinEvent checks that a room keeps its first join
// event and the join server of its first partial join, while later joins add
// to its servers.
func TestPartialStateTableJoinEvent(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		tab, cleanup := mustCreatePartialStateTable(t, dbType)
		defer cleanup()
		ctx := context.Background()
		roomNID := types.RoomNID(1)

		require.NoError(t, tab.InsertPartialStateRoom(ctx, nil, roomNID, 0, "server1.example.com", []string{"server1.example.com"}, 0))
		nid, err := tab.SelectPartialStateJoinEventNID(ctx, nil, roomNID)
		require.NoError(t, err)
		assert.Zero(t, nid)

		require.NoError(t, tab.UpdatePartialStateJoinEventNID(ctx, nil, roomNID, 100))
		require.NoError(t, tab.UpdatePartialStateJoinEventNID(ctx, nil, roomNID, 200))
		nid, err = tab.SelectPartialStateJoinEventNID(ctx, nil, roomNID)
		require.NoError(t, err)
		assert.Equal(t, types.EventNID(100), nid, "the first join stays the room's join")

		require.NoError(t, tab.InsertPartialStateRoom(ctx, nil, roomNID, 0, "server2.example.com", []string{"server2.example.com"}, 0))
		via, err := tab.SelectPartialStateJoinedVia(ctx, nil, roomNID)
		require.NoError(t, err)
		assert.Equal(t, "server1.example.com", via)
		servers, err := tab.SelectPartialStateServers(ctx, nil, roomNID)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"server1.example.com", "server2.example.com"}, servers)

		nid, err = tab.SelectPartialStateJoinEventNID(ctx, nil, types.RoomNID(2))
		require.NoError(t, err)
		assert.Zero(t, nid)
	})
}
