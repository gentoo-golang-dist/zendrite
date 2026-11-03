// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package storage_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	postgresdeltas "codefloe.com/pat-s/zendrite/syncapi/storage/postgres/deltas"
	sqlitedeltas "codefloe.com/pat-s/zendrite/syncapi/storage/sqlite3/deltas"
	"codefloe.com/pat-s/zendrite/test"
)

// The roomserver's repairs announced events sent months before, and the sync
// API wrote them at new stream positions, so they became the latest events of
// their rooms. The migration must keep them from sync timelines, and leave
// alone an event that arrived a little late, and the events written after
// one with a bogus depth.
func TestExcludeRepairedEvents(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		alice := test.NewUser(t)
		start := time.Now().Add(-90 * 24 * time.Hour)

		room := test.NewRoom(t, alice)
		repaired := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "accepted months later"}, test.WithTimestamp(start))
		repairedMember := room.CreateAndInsert(t, alice, "m.room.member", map[string]any{"membership": "join", "displayname": "Old"},
			test.WithStateKey(alice.ID), test.WithTimestamp(start.Add(time.Minute)))
		// The late event comes before the recent ones by depth, but arrives
		// after them.
		late := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "a little late"},
			test.WithTimestamp(time.Now().Add(-40*time.Minute)))
		var recent []*rstypes.HeaderedEvent
		for i := range 3 {
			recent = append(recent, room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "recent"},
				test.WithTimestamp(time.Now().Add(time.Duration(i-30)*time.Minute))))
		}

		bogus := test.NewRoom(t, alice)
		deep := bogus.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "bogus depth"})
		afterDeep := bogus.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "after the bogus depth"})

		connStr, closeDB := test.PrepareDBConnectionString(t, dbType)
		t.Cleanup(closeDB)
		db, err := storage.NewSyncServerDatasource(ctx, sqlutil.NewConnectionManager(nil, config.DatabaseOptions{}), &config.DatabaseOptions{
			ConnectionString: config.DataSource(connStr),
		})
		require.NoError(t, err)

		// Everything but the repaired events, in order, then a late event,
		// then the repaired events, as the repair announced them.
		events := room.Events()
		var inOrder []*rstypes.HeaderedEvent
		for _, ev := range events {
			if ev.EventID() != repaired.EventID() && ev.EventID() != repairedMember.EventID() && ev.EventID() != late.EventID() {
				inOrder = append(inOrder, ev)
			}
		}
		MustWriteEvents(t, db, inOrder)
		MustWriteEvents(t, db, []*rstypes.HeaderedEvent{late, repaired, repairedMember})
		MustWriteEvents(t, db, bogus.Events())

		driver, up := "pgx", postgresdeltas.UpExcludeRepairedEvents
		if dbType == test.DBTypeSQLite {
			driver, up = sqlutil.SQLITE_DRIVER_NAME, sqlitedeltas.UpExcludeRepairedEvents
		}
		raw, err := sql.Open(driver, connStr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = raw.Close() })
		_, err = raw.Exec(`UPDATE syncapi_output_room_events_topology SET topological_position = 1000000000 WHERE event_id = $1`, deep.EventID())
		require.NoError(t, err)

		tx, err := raw.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, up(ctx, tx))
		require.NoError(t, tx.Commit())

		excluded := func(ev *rstypes.HeaderedEvent) bool {
			t.Helper()
			var exclude bool
			require.NoError(t, raw.QueryRow(`SELECT exclude_from_sync FROM syncapi_output_room_events WHERE event_id = $1`, ev.EventID()).Scan(&exclude))
			return exclude
		}
		require.True(t, excluded(repaired), "the repaired message must be kept from sync")
		require.True(t, excluded(repairedMember), "the repaired membership must be kept from sync")
		require.False(t, excluded(late), "an event less than an hour late must stay in sync")
		for _, ev := range recent {
			require.False(t, excluded(ev), "an event written in order must stay in sync")
		}
		require.False(t, excluded(afterDeep), "an event after one with a bogus depth must stay in sync")
	})
}
