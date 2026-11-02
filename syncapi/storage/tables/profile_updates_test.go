// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package tables_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/storage/postgres"
	"codefloe.com/pat-s/zendrite/syncapi/storage/sqlite3"
	"codefloe.com/pat-s/zendrite/syncapi/storage/tables"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
)

func TestProfileUpdatesTable(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		connStr, closeDB := test.PrepareDBConnectionString(t, dbType)
		defer closeDB()
		db, err := sqlutil.Open(&config.DatabaseOptions{
			ConnectionString: config.DataSource(connStr),
		}, sqlutil.NewExclusiveWriter())
		require.NoError(t, err)
		var tab tables.ProfileUpdates
		switch dbType {
		case test.DBTypePostgres:
			tab, err = postgres.NewPostgresProfileUpdatesTable(db)
		case test.DBTypeSQLite:
			var stream sqlite3.StreamIDStatements
			require.NoError(t, stream.Prepare(db))
			tab, err = sqlite3.NewSqliteProfileUpdatesTable(db, &stream)
		}
		require.NoError(t, err)

		maxID, err := tab.SelectMaxProfileUpdateID(ctx, nil)
		require.NoError(t, err)
		require.Zero(t, maxID)

		var positions []types.StreamPosition
		for _, change := range [][2]string{
			{"@alice:server", "displayname"},
			{"@bob:server", "avatar_url"},
			{"@alice:server", "m.tz"},
		} {
			pos, insertErr := tab.InsertProfileUpdate(ctx, nil, change[0], change[1])
			require.NoError(t, insertErr)
			positions = append(positions, pos)
		}
		require.Less(t, positions[0], positions[1])
		require.Less(t, positions[1], positions[2])
		maxID, err = tab.SelectMaxProfileUpdateID(ctx, nil)
		require.NoError(t, err)
		require.Equal(t, int64(positions[2]), maxID)

		updates, err := tab.SelectProfileUpdatesInRange(ctx, nil, positions[0], positions[2], 10)
		require.NoError(t, err)
		require.Equal(t, []types.ProfileUpdate{
			{Position: positions[1], UserID: "@bob:server", Field: "avatar_url"},
			{Position: positions[2], UserID: "@alice:server", Field: "m.tz"},
		}, updates)

		updates, err = tab.SelectProfileUpdatesInRange(ctx, nil, 0, positions[2], 2)
		require.NoError(t, err)
		require.Len(t, updates, 2)
		require.Equal(t, positions[1], updates[1].Position)

		updates, err = tab.SelectProfileUpdatesInRange(ctx, nil, positions[2], positions[2], 10)
		require.NoError(t, err)
		require.Empty(t, updates)
	})
}
