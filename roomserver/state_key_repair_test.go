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

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

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

// Before #445, auth events fetched over federation were stored under the
// state key of the event they authorized. The startup repair must store them
// under their own key again and fix the memberships they hid.
//
// The room reproduces what was found on a production server: its join rules
// are stored under a user's state key, so the room looks invite-only and a
// join into it is rejected. Carol's join is stored under no state key, so the
// membership table says she left. Bob's latest join is stored under no state
// key too, and the current state still holds his leave before it: under his
// key, the join shares it with the leave, and state resolution must pick the
// join. Charlie's and Eve's joins, and the message Charlie sent after his,
// rejected because the join rules were missing, must be accepted, but Eve was
// banned after joining and must stay banned. Rejected outliers, which have no
// state to check them against, must stay rejected without stopping the repair.
func TestRepairStateKeys(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	charlie := test.NewUser(t)
	dave := test.NewUser(t)
	carol := test.NewUser(t)
	eve := test.NewUser(t)
	frank := test.NewUser(t)
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

		// The repair scheduled by the migration of the new database runs in
		// the background at startup. Let it finish before scheduling it again.
		require.Eventually(t, func() bool {
			_, pending, cerr := db.StateKeyRepairCursor(ctx)
			require.NoError(t, cerr)
			return !pending
		}, 10*time.Second, 10*time.Millisecond)

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		// Bob's events are a minute apart, so that state resolution orders
		// his leave before his rejoin by timestamp, as on a real server.
		start := time.Now()
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID), test.WithTimestamp(start))
		bobLeave := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(bob.ID), test.WithTimestamp(start.Add(time.Minute)))
		bobRejoin := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID), test.WithTimestamp(start.Add(2*time.Minute)))
		carolJoin := room.CreateAndInsert(t, carol, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(carol.ID))
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
		nids, err := db.EventNIDs(ctx, []string{joinRulesID, bobLeave.EventID(), bobRejoin.EventID(), carolJoin.EventID()})
		require.NoError(t, err)
		keys, err := db.EventStateKeyNIDs(ctx, []string{"", bob.ID, carol.ID})
		require.NoError(t, err)

		// The bug left full state snapshots, stored as one block, that hold a
		// member's join under no state key next to the leave it replaced
		// under the member's key. Build one with the room's state and bob's
		// leave, and make it the current state and the state before the
		// forward extremity.
		roomState := state.NewStateResolution(db, roomInfo, rsAPI)
		currentEntries, err := roomState.LoadStateAtSnapshot(ctx, roomInfo.StateSnapshotNID())
		require.NoError(t, err)
		leaveEntries, err := db.StateEntriesForEventIDs(ctx, []string{bobLeave.EventID()}, false)
		require.NoError(t, err)
		updater, err := db.GetRoomUpdater(ctx, roomInfo)
		require.NoError(t, err)
		oneBlock, err := updater.AddState(ctx, roomInfo.RoomNID, nil, append(currentEntries, leaveEntries...))
		require.NoError(t, err)
		latest := updater.LatestEvents()
		var lastEventNID types.EventNID
		for _, l := range latest {
			lastEventNID = max(lastEventNID, l.EventNID)
			require.NoError(t, updater.SetState(ctx, l.EventNID, oneBlock))
		}
		require.NoError(t, updater.SetLatestEvents(roomInfo.RoomNID, latest, lastEventNID, oneBlock))
		require.NoError(t, updater.Commit())

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
		_, err = sqlDB.ExecContext(ctx, "UPDATE roomserver_events SET event_state_key_nid = 0 WHERE event_nid = $1",
			nids[carolJoin.EventID()].EventNID)
		require.NoError(t, err)
		_, err = sqlDB.ExecContext(ctx, "UPDATE roomserver_membership SET membership_nid = 1 WHERE room_nid = $1 AND target_nid = $2",
			roomInfo.RoomNID, keys[carol.ID])
		require.NoError(t, err)
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
		// requireCarolRow checks the membership state recorded for carol: 1 is
		// leave and 3 is join.
		requireCarolRow := func(want int) {
			t.Helper()
			var membership int
			require.NoError(t, sqlDB.QueryRowContext(ctx,
				"SELECT membership_nid FROM roomserver_membership WHERE room_nid = $1 AND target_nid = $2",
				roomInfo.RoomNID, keys[carol.ID]).Scan(&membership))
			require.Equal(t, want, membership, "membership state of carol")
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
		// So is everything charlie sends after it.
		charlieEarlyMessage := room.CreateAndInsert(t, charlie, "m.room.message", map[string]any{"body": "is this on?"})
		_ = api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{charlieEarlyMessage}, "test", "test", "test", nil, false)
		requireRejected(charlieEarlyMessage, true)
		// Alice redacts another of them.
		charlieSecret := room.CreateAndInsert(t, charlie, "m.room.message", map[string]any{"body": "secret"})
		_ = api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{charlieSecret}, "test", "test", "test", nil, false)
		requireRejected(charlieSecret, true)
		redaction := room.CreateAndInsert(t, alice, spec.MRoomRedaction, map[string]any{}, test.WithRedacts(charlieSecret.EventID()))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{redaction}, "test", "test", "test", nil, false))
		// Eve's join is rejected the same way, and then she is banned.
		eveJoin := room.CreateAndInsert(t, eve, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(eve.ID))
		_ = api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{eveJoin}, "test", "test", "test", nil, false)
		requireRejected(eveJoin, true)
		// Frank's join and another of charlie's messages are rejected the
		// same way, but stored as outliers, with no state of their own, as
		// most rejected events on the production server are.
		frankJoin := room.CreateAndInsert(t, frank, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(frank.ID))
		_ = api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{frankJoin}, "test", "test", "test", nil, false)
		requireRejected(frankJoin, true)
		charlieOutlier := room.CreateAndInsert(t, charlie, "m.room.message", map[string]any{"body": "lost"})
		_ = api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{charlieOutlier}, "test", "test", "test", nil, false)
		requireRejected(charlieOutlier, true)
		eveBan := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Ban}, test.WithStateKey(eve.ID))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{eveBan}, "test", "test", "test", nil, false))
		requireRejected(eveBan, false)
		outlierNIDs, err := db.EventNIDs(ctx, []string{frankJoin.EventID(), charlieOutlier.EventID()})
		require.NoError(t, err)
		for _, nid := range outlierNIDs {
			_, err = sqlDB.ExecContext(ctx, "UPDATE roomserver_events SET state_snapshot_nid = 0 WHERE event_nid = $1", nid.EventNID)
			require.NoError(t, err)
		}
		requireMembership(bob, spec.Leave)
		requireCarolRow(1)
		before, err := db.GetStateEvent(ctx, room.ID, spec.MRoomMember, bob.ID)
		require.NoError(t, err)
		require.NotNil(t, before)
		require.Equal(t, bobLeave.EventID(), before.EventID(), "the current state must hold bob's leave before the repair")

		outputs, err := nc.SubscribeSync(cfg.Global.JetStream.Prefixed(jetstream.OutputRoomEvent))
		require.NoError(t, err)
		defer outputs.Unsubscribe() //nolint:errcheck

		internalAPI.RepairStateKeys(ctx)

		// Charlie's join is announced as adding itself to the current state.
		// Eve's join is not, since the ban stays in the current state.
		var announcedCharlie, announcedEarlyMessage, announcedSecret bool
		for {
			msg, nextErr := outputs.NextMsg(2 * time.Second)
			if nextErr != nil {
				break
			}
			var output api.OutputEvent
			require.NoError(t, json.Unmarshal(msg.Data, &output))
			if output.Type == api.OutputTypeOldRoomEvent {
				switch output.OldRoomEvent.Event.EventID() {
				case charlieEarlyMessage.EventID():
					announcedEarlyMessage = true
				case charlieOutlier.EventID():
					t.Fatal("charlie's outlier message must not be announced")
				case charlieSecret.EventID():
					// The redaction that arrived while it was rejected applies.
					announcedSecret = true
					require.False(t, gjson.GetBytes(output.OldRoomEvent.Event.Content(), "body").Exists(),
						"the redacted message must be announced redacted: %s", output.OldRoomEvent.Event.Content())
				}
				continue
			}
			if output.Type != api.OutputTypeNewRoomEvent {
				continue
			}
			switch output.NewRoomEvent.Event.EventID() {
			case charlieJoin.EventID():
				announcedCharlie = true
				require.Equal(t, []string{charlieJoin.EventID()}, output.NewRoomEvent.AddsStateEventIDs)
				require.Equal(t, string(api.DoNotSendToOtherServers), output.NewRoomEvent.SendAsServer)
			case eveJoin.EventID():
				t.Fatal("eve's join must not be announced, since the ban stays her membership")
			case frankJoin.EventID():
				t.Fatal("frank's join must not be announced, since it is an outlier")
			}
		}
		require.True(t, announcedCharlie, "charlie's accepted join must be announced")
		require.True(t, announcedEarlyMessage, "charlie's accepted message must be announced as an old event")
		require.True(t, announcedSecret, "charlie's redacted message must be announced as an old event")

		_, pending, err := db.StateKeyRepairCursor(ctx)
		require.NoError(t, err)
		require.False(t, pending, "the repair must finish")

		// The current state and the state before the forward extremity no
		// longer hold bob's membership twice.
		roomInfo, err = db.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		repairedState := state.NewStateResolution(db, roomInfo, rsAPI)
		snapshots := []types.StateSnapshotNID{roomInfo.StateSnapshotNID()}
		latestNIDs, _, _, err := db.LatestEventIDs(ctx, roomInfo.RoomNID)
		require.NoError(t, err)
		require.NotEmpty(t, latestNIDs)
		atLatest, err := db.StateAtEventIDs(ctx, latestNIDs)
		require.NoError(t, err)
		for _, l := range atLatest {
			snapshots = append(snapshots, l.BeforeStateSnapshotNID)
		}
		for _, snapshot := range snapshots {
			require.NotEqual(t, oneBlock, snapshot, "the snapshot with bob's membership twice must be replaced")
			conflicts, cerr := repairedState.ResolveSnapshotConflicts(ctx, snapshot)
			require.NoError(t, cerr)
			require.Nil(t, conflicts, "snapshot %d must hold every key once", snapshot)
		}
		entries, err := db.StateEntriesForEventIDs(ctx, []string{joinRulesID, bobRejoin.EventID(), carolJoin.EventID()}, false)
		require.NoError(t, err)
		require.Len(t, entries, 3)
		byNID := map[types.EventNID]types.EventStateKeyNID{}
		for _, entry := range entries {
			byNID[entry.EventNID] = entry.EventStateKeyNID
		}
		require.Equal(t, keys[""], byNID[nids[joinRulesID].EventNID], "the join rules must be stored under the empty state key")
		require.Equal(t, keys[carol.ID], byNID[nids[carolJoin.EventID()].EventNID], "carol's join must be stored under her state key")
		require.Equal(t, keys[bob.ID], byNID[nids[bobRejoin.EventID()].EventNID], "bob's join must be stored under his state key")
		requireCarolRow(3)
		requireMembership(bob, spec.Join)
		current, err := db.GetStateEvent(ctx, room.ID, spec.MRoomMember, bob.ID)
		require.NoError(t, err)
		require.NotNil(t, current)
		require.Equal(t, bobRejoin.EventID(), current.EventID())

		// Charlie's join, rejected because the join rules were missing, is
		// accepted and in the current state, and his next message too. Eve's
		// join is accepted as well, but the ban after it stays her
		// membership.
		requireRejected(charlieJoin, false)
		requireRejected(charlieEarlyMessage, false)
		requireMembership(charlie, spec.Join)
		current, err = db.GetStateEvent(ctx, room.ID, spec.MRoomMember, charlie.ID)
		require.NoError(t, err)
		require.NotNil(t, current)
		require.Equal(t, charlieJoin.EventID(), current.EventID())
		requireRejected(eveJoin, false)
		requireMembership(eve, spec.Ban)
		// Outliers have no state to check them against, so they stay
		// rejected.
		requireRejected(frankJoin, true)
		requireRejected(charlieOutlier, true)
		current, err = db.GetStateEvent(ctx, room.ID, spec.MRoomMember, eve.ID)
		require.NoError(t, err)
		require.NotNil(t, current)
		require.Equal(t, eveBan.EventID(), current.EventID())
		charlieMessage := room.CreateAndInsert(t, charlie, "m.room.message", map[string]any{"body": "hi"})
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{charlieMessage}, "test", "test", "test", nil, false))
		requireRejected(charlieMessage, false)

		// The room is public again, so a new join is accepted.
		daveJoin := room.CreateAndInsert(t, dave, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(dave.ID))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{daveJoin}, "test", "test", "test", nil, false))
		requireRejected(daveJoin, false)
		requireMembership(dave, spec.Join)

		// The state new events are built on holds bob's join too.
		bobMessage := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "back"})
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{bobMessage}, "test", "test", "test", nil, false))
		requireRejected(bobMessage, false)
		requireMembership(bob, spec.Join)
	})
}
