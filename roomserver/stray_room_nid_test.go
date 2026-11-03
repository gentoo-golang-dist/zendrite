// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"database/sql"
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

// A membership updater for a room the server does not know yet assigns the
// room a NID inside its transaction. When that transaction is rolled back,
// the NID must not stay cached, or the room's events are stored under a room
// NID that no longer exists.
func TestRolledBackRoomNIDIsNotCached(t *testing.T) {
	alice := test.NewUser(t)
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
		defer closeRig()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches) //nolint:contextcheck
		require.NoError(t, err)
		sqlDB, _, err := cm.Connection(&cfg.RoomServer.Database)
		require.NoError(t, err)

		room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
		updater, err := db.MembershipUpdater(ctx, room.ID, alice.ID, true, room.Version)
		require.NoError(t, err)
		require.NoError(t, updater.Rollback())

		roomInfo, err := db.GetOrCreateRoomInfo(ctx, room.Events()[0].PDU)
		require.NoError(t, err)
		var roomID string
		require.NoError(t, sqlDB.QueryRowContext(ctx,
			"SELECT room_id FROM roomserver_rooms WHERE room_nid = $1", roomInfo.RoomNID).Scan(&roomID),
			"the room NID must exist")
		require.Equal(t, room.ID, roomID)
	})
}

// A server upgraded from Dendrite stored the create event of a room under a
// room NID that no longer exists, so every event of the room cites an auth
// event the recheck of rejected events cannot find in the room. The recheck
// must still check the rooms after it and finish, and the repair must move
// the event back to its room and check the room again.
func TestRecheckWithStrayEvent(t *testing.T) {
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

		// The repairs scheduled by the migrations of the new database run in
		// the background at startup. Let them finish first.
		require.Eventually(t, func() bool {
			_, repairPending, cerr := db.StateKeyRepairCursor(ctx)
			require.NoError(t, cerr)
			_, recheckPending, cerr := db.RejectedRecheckCursor(ctx)
			require.NoError(t, cerr)
			return !repairPending && !recheckPending
		}, 10*time.Second, 10*time.Millisecond)

		sqlDB, _, err := cm.Connection(&cfg.RoomServer.Database)
		require.NoError(t, err)

		// Two rooms, each with a message that is rejected. The create event
		// of the first is stored under a room NID that does not exist.
		newRoom := func() (*test.Room, *types.HeaderedEvent) {
			room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
			message := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "hello"})
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
			_, err = sqlDB.ExecContext(ctx, "UPDATE roomserver_events SET is_rejected = TRUE WHERE event_id = $1", message.EventID())
			require.NoError(t, err)
			return room, message
		}
		strayRoom, strayMessage := newRoom()
		_, otherMessage := newRoom()
		strayInfo, err := db.RoomInfo(ctx, strayRoom.ID)
		require.NoError(t, err)
		create := strayRoom.Events()[0]
		_, err = sqlDB.ExecContext(ctx, "UPDATE roomserver_events SET room_nid = $1 WHERE event_id = $2", 999999, create.EventID())
		require.NoError(t, err)

		requireRejected := func(ev *types.HeaderedEvent, want bool) {
			t.Helper()
			var rejected bool
			require.NoError(t, sqlDB.QueryRowContext(ctx,
				"SELECT is_rejected FROM roomserver_events WHERE event_id = $1", ev.EventID()).Scan(&rejected))
			require.Equal(t, want, rejected, "rejection of %s", ev.EventID())
		}
		recheck := func(schedule func(context.Context, *sql.Tx) error) {
			t.Helper()
			tx, terr := sqlDB.BeginTx(ctx, nil)
			require.NoError(t, terr)
			require.NoError(t, schedule(ctx, tx))
			require.NoError(t, tx.Commit())
			internalAPI.RecheckRejectedEvents(ctx)
		}

		schedule, repair := sqlitedeltas.UpRejectedRecheck, sqlitedeltas.UpRepairStrayEvents
		if dbType == test.DBTypePostgres {
			schedule, repair = pgdeltas.UpRejectedRecheck, pgdeltas.UpRepairStrayEvents
		}

		// The room fails the same way on every start, so the recheck goes
		// past it and finishes. The repair schedules it again.
		recheck(schedule)
		requireRejected(otherMessage, false)
		requireRejected(strayMessage, true)
		_, pending, err := db.RejectedRecheckCursor(ctx)
		require.NoError(t, err)
		require.False(t, pending, "a room whose stored data is inconsistent must not keep the recheck pending")

		recheck(repair)
		var roomNID types.RoomNID
		require.NoError(t, sqlDB.QueryRowContext(ctx,
			"SELECT room_nid FROM roomserver_events WHERE event_id = $1", create.EventID()).Scan(&roomNID))
		require.Equal(t, strayInfo.RoomNID, roomNID, "the create event must be moved back to its room")
		requireRejected(strayMessage, false)
		_, pending, err = db.RejectedRecheckCursor(ctx)
		require.NoError(t, err)
		require.False(t, pending, "the recheck must finish")

		// A rejected event under a room NID that does not exist, whose room
		// the repair cannot find either, leaves nothing to check. The
		// recheck must skip it rather than check it again on every start.
		_, err = sqlDB.ExecContext(ctx, "UPDATE roomserver_events SET room_nid = $1, is_rejected = TRUE WHERE event_id = $2",
			888888, otherMessage.EventID())
		require.NoError(t, err)
		recheck(schedule)
		_, pending, err = db.RejectedRecheckCursor(ctx)
		require.NoError(t, err)
		require.False(t, pending, "a room that does not exist must not keep the recheck pending")
	})
}
