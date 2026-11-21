// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package tables_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/storage/postgres"
	"codefloe.com/pat-s/zendrite/syncapi/storage/sqlite3"
	"codefloe.com/pat-s/zendrite/syncapi/storage/tables"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
)

func newUnPartialStatedRoomsTable(t *testing.T, dbType test.DBType) (tables.UnPartialStatedRooms, *sql.DB, func()) {
	t.Helper()
	connStr, closeDB := test.PrepareDBConnectionString(t, dbType)
	db, err := sqlutil.Open(&config.DatabaseOptions{
		ConnectionString: config.DataSource(connStr),
	}, sqlutil.NewExclusiveWriter())
	require.NoError(t, err)

	var tab tables.UnPartialStatedRooms
	switch dbType {
	case test.DBTypePostgres:
		// The events table creates the PDU stream's sequence.
		_, err = postgres.NewPostgresEventsTable(context.Background(), db)
		require.NoError(t, err)
		tab, err = postgres.NewPostgresUnPartialStatedRoomsTable(db)
	case test.DBTypeSQLite:
		var stream sqlite3.StreamIDStatements
		require.NoError(t, stream.Prepare(db))
		tab, err = sqlite3.NewSqliteUnPartialStatedRoomsTable(db, &stream)
	}
	require.NoError(t, err)
	return tab, db, closeDB
}

// TestPartialStateJoinLeftByOlderBuild checks that a pending partial-state
// join (MSC3706) does not outlive the un-partial-stated records written after
// it. A build that does not know syncapi_partial_state_joins, which a rollback
// can hand the deployment to between the join and the end of its resync,
// writes the records and leaves the join behind; the room must not stay out of
// eager syncs for good once a newer build takes over again.
func TestPartialStateJoinLeftByOlderBuild(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		room := test.NewRoom(t, alice)
		tab, db, closeDB := newUnPartialStatedRoomsTable(t, dbType) //nolint:contextcheck
		defer closeDB()

		partialRooms := func(to types.StreamPosition) (roomIDs []string) {
			t.Helper()
			require.NoError(t, sqlutil.WithTransaction(db, func(txn *sql.Tx) (err error) {
				roomIDs, err = tab.SelectPartialStateRooms(ctx, txn, alice.ID, to)
				return err
			}))
			return roomIDs
		}
		write := func(f func(txn *sql.Tx) error) {
			t.Helper()
			require.NoError(t, sqlutil.WithTransaction(db, f))
		}
		unPartialStated := func() (pos types.StreamPosition) {
			t.Helper()
			write(func(txn *sql.Tx) (err error) {
				pos, err = tab.InsertUnPartialStatedRoom(ctx, txn, room.ID, alice.ID)
				return err
			})
			return pos
		}

		// The room's previous resync, long before the join.
		earlier := unPartialStated()
		write(func(txn *sql.Tx) error { return tab.InsertPartialStateJoin(ctx, txn, room.ID) })
		assert.Equal(t, []string{room.ID}, partialRooms(earlier), "records before the join leave it pending")
		farAhead := earlier + 1000
		assert.Equal(t, []string{room.ID}, partialRooms(farAhead))

		// The older build writes the records but keeps the join.
		pos := unPartialStated()
		assert.Equal(t, []string{room.ID}, partialRooms(pos-1), "until the sync reaches the user's record")
		assert.Empty(t, partialRooms(pos), "the records settle the join they follow")

		// A new partial join is pending again, whatever records came before.
		write(func(txn *sql.Tx) error { return tab.InsertPartialStateJoin(ctx, txn, room.ID) })
		assert.Equal(t, []string{room.ID}, partialRooms(pos))
		assert.Equal(t, []string{room.ID}, partialRooms(farAhead+1000))
	})
}
