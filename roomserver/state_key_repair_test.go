// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
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

// Before #445, auth events fetched over federation were stored under the
// state key of the event they authorized. The startup repair must store them
// under their own key again and fix the memberships they hid.
//
// The room reproduces what was found on a production server: its join rules
// are stored under a user's state key, so the room looks invite-only and a
// join into it is rejected, and a member's latest join is stored under no
// state key, so the membership table still has the leave before it.
func TestRepairStateKeys(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	charlie := test.NewUser(t)
	dave := test.NewUser(t)
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

		// The repair scheduled by the migration of the new database runs in
		// the background at startup. Let it finish before scheduling it again.
		require.Eventually(t, func() bool {
			_, pending, cerr := db.StateKeyRepairCursor(ctx)
			require.NoError(t, cerr)
			return !pending
		}, 10*time.Second, 10*time.Millisecond)

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		bobLeave := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(bob.ID))
		bobRejoin := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))

		var joinRulesID string
		for _, ev := range room.Events() {
			if ev.Type() == spec.MRoomJoinRules {
				joinRulesID = ev.EventID()
			}
		}
		require.NotEmpty(t, joinRulesID)
		roomInfo, err := db.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		nids, err := db.EventNIDs(ctx, []string{joinRulesID, bobLeave.EventID(), bobRejoin.EventID()})
		require.NoError(t, err)
		keys, err := db.EventStateKeyNIDs(ctx, []string{"", bob.ID})
		require.NoError(t, err)

		// Store the events the way the bug did, and schedule the repair
		// again, as the upgrade migration does.
		sqlDB, _, err := cm.Connection(&cfg.RoomServer.Database)
		require.NoError(t, err)
		_, err = sqlDB.ExecContext(ctx, "UPDATE roomserver_events SET event_state_key_nid = $1 WHERE event_nid = $2",
			keys[bob.ID], nids[joinRulesID].EventNID)
		require.NoError(t, err)
		_, err = sqlDB.ExecContext(ctx, "UPDATE roomserver_events SET event_state_key_nid = 0 WHERE event_nid = $1",
			nids[bobRejoin.EventID()].EventNID)
		require.NoError(t, err)
		_, err = sqlDB.ExecContext(ctx, "UPDATE roomserver_membership SET membership_nid = 1, event_nid = $1 WHERE room_nid = $2 AND target_nid = $3",
			nids[bobLeave.EventID()].EventNID, roomInfo.RoomNID, keys[bob.ID])
		require.NoError(t, err)
		// A join stored under no state key did not replace the leave before
		// it, so the current state holds both.
		leaveEntries, err := db.StateEntriesForEventIDs(ctx, []string{bobLeave.EventID()}, false)
		require.NoError(t, err)
		updater, err := db.GetRoomUpdater(ctx, roomInfo)
		require.NoError(t, err)
		blocks, err := updater.StateBlockNIDs(ctx, []types.StateSnapshotNID{updater.CurrentStateSnapshotNID()})
		require.NoError(t, err)
		withLeave, err := updater.AddState(ctx, roomInfo.RoomNID, blocks[0].StateBlockNIDs, leaveEntries)
		require.NoError(t, err)
		latest := updater.LatestEvents()
		var lastEventNID types.EventNID
		for _, l := range latest {
			lastEventNID = max(lastEventNID, l.EventNID)
		}
		require.NoError(t, updater.SetLatestEvents(roomInfo.RoomNID, latest, lastEventNID, withLeave))
		require.NoError(t, updater.Commit())

		tx, err := sqlDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		if dbType == test.DBTypePostgres {
			require.NoError(t, pgdeltas.UpStateKeyRepair(ctx, tx))
		} else {
			require.NoError(t, sqlitedeltas.UpStateKeyRepair(ctx, tx))
		}
		require.NoError(t, tx.Commit())

		requireMembership := func(user *test.User, want string) {
			t.Helper()
			userID, uerr := spec.NewUserID(user.ID, true)
			require.NoError(t, uerr)
			res := api.QueryMembershipForUserResponse{}
			require.NoError(t, rsAPI.QueryMembershipForUser(ctx, &api.QueryMembershipForUserRequest{RoomID: room.ID, UserID: *userID}, &res))
			require.Equal(t, want, res.Membership, "membership of %s", user.ID)
		}
		requireRejected := func(ev *types.HeaderedEvent, want bool) {
			t.Helper()
			rejected, rerr := db.IsEventRejected(ctx, roomInfo.RoomNID, ev.EventID())
			require.NoError(t, rerr)
			require.Equal(t, want, rejected, "rejection of %s", ev.EventID())
		}

		// Without the join rules under their key the public room looks
		// invite-only, so a join is rejected.
		charlieJoin := room.CreateAndInsert(t, charlie, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(charlie.ID))
		_ = api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{charlieJoin}, "test", "test", "test", nil, false)
		requireRejected(charlieJoin, true)
		requireMembership(bob, spec.Leave)

		internalAPI.RepairStateKeys(ctx)

		_, pending, err := db.StateKeyRepairCursor(ctx)
		require.NoError(t, err)
		require.False(t, pending, "the repair must finish")
		entries, err := db.StateEntriesForEventIDs(ctx, []string{joinRulesID, bobRejoin.EventID()}, false)
		require.NoError(t, err)
		require.Len(t, entries, 2)
		for _, entry := range entries {
			switch entry.EventTypeNID {
			case types.MRoomJoinRulesNID:
				require.Equal(t, keys[""], entry.EventStateKeyNID, "the join rules must be stored under the empty state key")
			case types.MRoomMemberNID:
				require.Equal(t, keys[bob.ID], entry.EventStateKeyNID, "bob's join must be stored under his state key")
			}
		}
		// Bob's join is now in the current state under his key next to the
		// older leave, and as the newer event it wins.
		requireMembership(bob, spec.Join)
		current, err := db.GetStateEvent(ctx, room.ID, spec.MRoomMember, bob.ID)
		require.NoError(t, err)
		require.NotNil(t, current)
		require.Equal(t, bobRejoin.EventID(), current.EventID())

		// The room is public again. Charlie's join stays rejected, since the
		// repair does not reprocess rejected events, but a new join is
		// accepted.
		requireRejected(charlieJoin, true)
		daveJoin := room.CreateAndInsert(t, dave, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(dave.ID))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{daveJoin}, "test", "test", "test", nil, false))
		requireRejected(daveJoin, false)
		requireMembership(dave, spec.Join)
	})
}
