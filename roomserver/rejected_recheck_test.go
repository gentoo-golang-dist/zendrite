// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	rsinternal "codefloe.com/pat-s/zendrite/roomserver/internal"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/storage"
	pgdeltas "codefloe.com/pat-s/zendrite/roomserver/storage/postgres/deltas"
	sqlitedeltas "codefloe.com/pat-s/zendrite/roomserver/storage/sqlite3/deltas"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// The state key repair accepted memberships that state stored under the
// wrong key had rejected, but on a production server it left events that
// depend on them rejected: the messages of a member whose join it accepted in
// an earlier pass, and a restricted join authorized by a user whose own join
// it accepted. The recheck must accept them, and must leave a message
// rejected whose sender was banned before it.
//
// The room is restricted. Wolf joins, leaves, joins again and sends two
// messages. Amy joins, and Xan joins with Amy's authorisation. The test stores
// the state before Wolf's messages and Xan's join as it was calculated while
// Wolf's and Amy's joins were rejected, and marks the events rejected. Yuki
// is banned, and her message after the ban cites her earlier join.
func TestRecheckRejectedEvents(t *testing.T) {
	alice := test.NewUser(t)
	wolf := test.NewUser(t)
	amy := test.NewUser(t)
	xan := test.NewUser(t)
	yuki := test.NewUser(t)
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
		defer closeRig()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches) //nolint:contextcheck
		require.NoError(t, err)

		natsInstance := jetstream.NATSInstance{}
		jsCtx, nc := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
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

		send := func(ev *types.HeaderedEvent) {
			t.Helper()
			_ = api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{ev}, "test", "test", "test", nil, false)
		}
		requireRejected := func(ev *types.HeaderedEvent, want bool) {
			t.Helper()
			roomInfo, rerr := db.RoomInfo(ctx, ev.RoomID().String())
			require.NoError(t, rerr)
			rejected, rerr := db.IsEventRejected(ctx, roomInfo.RoomNID, ev.EventID())
			require.NoError(t, rerr)
			require.Equal(t, want, rejected, "rejection of %s", ev.EventID())
		}
		via := func(user *test.User) map[string]any {
			return map[string]any{
				"membership":                       spec.Join,
				"join_authorised_via_users_server": user.ID, //nolint:misspell // Matrix spec uses British spelling
			}
		}

		room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
		room.CreateAndInsert(t, alice, spec.MRoomJoinRules, map[string]any{
			"join_rule": spec.Restricted,
			"allow":     []map[string]any{{"room_id": "!elsewhere:test", "type": spec.MRoomMembership}},
		}, test.WithStateKey(""))
		yukiJoin := room.CreateAndInsert(t, yuki, spec.MRoomMember, via(alice), test.WithStateKey(yuki.ID))
		// Wolf's events are a minute apart, so that state resolution orders
		// his leave before his rejoin by timestamp, as on a real server.
		start := time.Now().Add(-time.Hour)
		room.CreateAndInsert(t, wolf, spec.MRoomMember, via(alice), test.WithStateKey(wolf.ID), test.WithTimestamp(start))
		wolfLeave := room.CreateAndInsert(t, wolf, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(wolf.ID), test.WithTimestamp(start.Add(time.Minute)))
		wolfRejoin := room.CreateAndInsert(t, wolf, spec.MRoomMember, via(alice), test.WithStateKey(wolf.ID), test.WithTimestamp(start.Add(2*time.Minute)))
		wolfFirst := room.CreateAndInsert(t, wolf, "m.room.message", map[string]any{"body": "first"}, test.WithTimestamp(start.Add(3*time.Minute)))
		wolfSecond := room.CreateAndInsert(t, wolf, "m.room.message", map[string]any{"body": "second"}, test.WithTimestamp(start.Add(4*time.Minute)))
		amyJoin := room.CreateAndInsert(t, amy, spec.MRoomMember, via(alice), test.WithStateKey(amy.ID))
		room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "welcome"})
		xanJoin := room.CreateAndInsert(t, xan, spec.MRoomMember, via(amy), test.WithStateKey(xan.ID))
		// Yuki's message follows her ban but cites her join. The test room
		// refuses to create it after the ban, so create it first.
		yukiBan := room.CreateEvent(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Ban}, test.WithStateKey(yuki.ID))
		yukiMessage := room.CreateEvent(t, yuki, "m.room.message", map[string]any{"body": "still here"}, test.WithPrevEvents(yukiBan.EventID()))
		room.InsertEvent(t, yukiBan)
		room.InsertEvent(t, yukiMessage)
		require.Contains(t, yukiMessage.AuthEventIDs(), yukiJoin.EventID())
		for _, ev := range room.Events() {
			send(ev)
		}
		for _, ev := range room.Events() {
			requireRejected(ev, ev.EventID() == yukiMessage.EventID())
		}

		// Leave the room as the state key repair left it on the production
		// server. The state stored before Wolf's messages was calculated
		// while his rejoin was rejected, so it holds his leave, and the state
		// before Xan's join misses Amy's join. The repair accepted both
		// joins, but the events that depend on them stay rejected.
		roomInfo, err := db.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		roomState := state.NewStateResolution(db, roomInfo, rsAPI)
		sqlDB, _, err := cm.Connection(&cfg.RoomServer.Database)
		require.NoError(t, err)
		leaveEntries, err := db.StateEntriesForEventIDs(ctx, []string{wolfLeave.EventID()}, false)
		require.NoError(t, err)
		amyEntries, err := db.StateEntriesForEventIDs(ctx, []string{amyJoin.EventID()}, false)
		require.NoError(t, err)
		rewrite := func(ev *types.HeaderedEvent, change func(types.StateEntry) (types.StateEntry, bool)) {
			t.Helper()
			at, aerr := db.StateAtEventIDs(ctx, []string{ev.EventID()})
			require.NoError(t, aerr)
			entries, lerr := roomState.LoadStateAtSnapshot(ctx, at[0].BeforeStateSnapshotNID)
			require.NoError(t, lerr)
			var changed []types.StateEntry
			for _, entry := range entries {
				if entry, keep := change(entry); keep {
					changed = append(changed, entry)
				}
			}
			updater, uerr := db.GetRoomUpdater(ctx, roomInfo)
			require.NoError(t, uerr)
			snapshot, serr := updater.AddState(ctx, roomInfo.RoomNID, nil, changed)
			require.NoError(t, serr)
			require.NoError(t, updater.Commit())
			_, err = sqlDB.ExecContext(ctx, "UPDATE roomserver_events SET state_snapshot_nid = $1, is_rejected = TRUE WHERE event_nid = $2",
				snapshot, at[0].EventNID)
			require.NoError(t, err)
		}
		withLeave := func(entry types.StateEntry) (types.StateEntry, bool) {
			if entry.StateKeyTuple == leaveEntries[0].StateKeyTuple {
				return leaveEntries[0], true
			}
			return entry, true
		}
		rewrite(wolfFirst, withLeave)
		rewrite(wolfSecond, withLeave)
		rewrite(xanJoin, func(entry types.StateEntry) (types.StateEntry, bool) {
			return entry, entry.StateKeyTuple != amyEntries[0].StateKeyTuple
		})
		requireRejected(wolfRejoin, false)
		requireRejected(wolfFirst, true)
		requireRejected(wolfSecond, true)
		requireRejected(xanJoin, true)

		// Schedule the recheck, as its migration does.
		tx, err := sqlDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		if dbType == test.DBTypePostgres {
			require.NoError(t, pgdeltas.UpRejectedRecheck(ctx, tx))
		} else {
			require.NoError(t, sqlitedeltas.UpRejectedRecheck(ctx, tx))
		}
		require.NoError(t, tx.Commit())

		outputs, err := nc.SubscribeSync(cfg.Global.JetStream.Prefixed(jetstream.OutputRoomEvent))
		require.NoError(t, err)
		defer outputs.Unsubscribe() //nolint:errcheck

		internalAPI.RecheckRejectedEvents(ctx)

		_, pending, err := db.RejectedRecheckCursor(ctx)
		require.NoError(t, err)
		require.False(t, pending, "the recheck must finish")

		requireRejected(wolfFirst, false)
		requireRejected(wolfSecond, false)
		requireRejected(xanJoin, false)
		requireRejected(yukiMessage, true)

		announced := map[string]bool{}
		for {
			msg, nextErr := outputs.NextMsg(2 * time.Second)
			if nextErr != nil {
				break
			}
			var output api.OutputEvent
			require.NoError(t, json.Unmarshal(msg.Data, &output))
			if output.Type == api.OutputTypeOldRoomEvent {
				announced[output.OldRoomEvent.Event.EventID()] = true
			}
		}
		require.True(t, announced[wolfFirst.EventID()], "wolf's first message must be announced")
		require.True(t, announced[wolfSecond.EventID()], "wolf's second message must be announced")
		require.False(t, announced[yukiMessage.EventID()], "yuki's message must not be announced")
	})
}
