// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	rsinternal "codefloe.com/pat-s/zendrite/roomserver/internal"
	"codefloe.com/pat-s/zendrite/roomserver/storage"
	pgdeltas "codefloe.com/pat-s/zendrite/roomserver/storage/postgres/deltas"
	sqlitedeltas "codefloe.com/pat-s/zendrite/roomserver/storage/sqlite3/deltas"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// A database a Dendrite purge left half deleted holds rejected events whose
// JSON is gone, events whose state snapshot is gone, and rooms whose current
// state is gone. The recheck of rejected events must leave the events it
// cannot check rejected, check the other events of their rooms, and finish
// rather than fail the same way on every start.
func TestRecheckWithMissingData(t *testing.T) {
	alice := test.NewUser(t)
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
		defer closeRig()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches) //nolint:contextcheck
		require.NoError(t, err)

		natsInstance := jetstream.NATSInstance{}
		jsCtx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsCtx, &cfg.Global.JetStream)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		internalAPI, ok := rsAPI.(*rsinternal.RoomserverInternalAPI)
		require.True(t, ok)

		require.Eventually(t, func() bool {
			_, repairPending, cerr := db.StateKeyRepairCursor(ctx)
			require.NoError(t, cerr)
			_, recheckPending, cerr := db.RejectedRecheckCursor(ctx)
			require.NoError(t, cerr)
			return !repairPending && !recheckPending
		}, 10*time.Second, 10*time.Millisecond)

		sqlDB, _, err := cm.Connection(&cfg.RoomServer.Database)
		require.NoError(t, err)
		exec := func(query string, args ...any) {
			t.Helper()
			_, eerr := sqlDB.ExecContext(ctx, query, args...)
			require.NoError(t, eerr)
		}
		eventNID := func(ev *types.HeaderedEvent) types.EventNID {
			t.Helper()
			var nid types.EventNID
			require.NoError(t, sqlDB.QueryRowContext(ctx,
				"SELECT event_nid FROM roomserver_events WHERE event_id = $1", ev.EventID()).Scan(&nid))
			return nid
		}
		requireRejected := func(ev *types.HeaderedEvent, want bool) {
			t.Helper()
			var rejected bool
			require.NoError(t, sqlDB.QueryRowContext(ctx,
				"SELECT is_rejected FROM roomserver_events WHERE event_id = $1", ev.EventID()).Scan(&rejected))
			require.Equal(t, want, rejected, "rejection of %s", ev.EventID())
		}
		message := func(room *test.Room) *types.HeaderedEvent {
			return room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "hello"})
		}

		// In the first room, one rejected message passes, one follows an
		// event whose state snapshot is gone, and one has no JSON.
		room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
		passes := message(room)
		noSnapshot := message(room)
		afterNoSnapshot := message(room)
		noJSON := message(room)
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))

		// In the second room, the current state is gone.
		broken := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
		brokenMessage := message(broken)
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, broken.Events(), "test", "test", "test", nil, false))

		for _, ev := range []*types.HeaderedEvent{passes, afterNoSnapshot, noJSON, brokenMessage} {
			exec("UPDATE roomserver_events SET is_rejected = TRUE WHERE event_id = $1", ev.EventID())
		}
		exec("UPDATE roomserver_events SET state_snapshot_nid = $1 WHERE event_id = $2", 999999, noSnapshot.EventID())
		nid := eventNID(noJSON)
		exec("DELETE FROM roomserver_event_json WHERE event_nid = $1", nid)
		caches.InvalidateRoomServerEvent(nid)
		exec("UPDATE roomserver_rooms SET state_snapshot_nid = $1 WHERE room_id = $2", 999998, broken.ID)
		caches.InvalidateRoom(broken.ID)

		schedule := sqlitedeltas.UpRejectedRecheck
		if dbType == test.DBTypePostgres {
			schedule = pgdeltas.UpRejectedRecheck
		}
		tx, err := sqlDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, schedule(ctx, tx))
		require.NoError(t, tx.Commit())
		internalAPI.RecheckRejectedEvents(ctx)

		requireRejected(passes, false)
		requireRejected(afterNoSnapshot, true)
		requireRejected(noJSON, true)
		requireRejected(brokenMessage, true)
		_, pending, err := db.RejectedRecheckCursor(ctx)
		require.NoError(t, err)
		require.False(t, pending, "missing data must not keep the recheck pending")
	})
}
