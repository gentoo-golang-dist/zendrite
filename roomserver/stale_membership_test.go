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
	"codefloe.com/pat-s/zendrite/roomserver/storage"
	pgdeltas "codefloe.com/pat-s/zendrite/roomserver/storage/postgres/deltas"
	sqlitedeltas "codefloe.com/pat-s/zendrite/roomserver/storage/sqlite3/deltas"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// On a production server memberships that arrived only as auth events of
// later events were stored as outliers and never entered the room's state:
// members who had left or been banned stayed joined, and members who had
// rejoined stayed out. The recheck must add the memberships that follow the
// current one for their user and pass against the current state, and leave
// the others out.
//
// Bob leaves, Carol is banned by Alice, and Dave, who had left, rejoins the
// public room; their memberships arrive as outliers. Helen was banned in the
// state, and her unban and rejoin arrive as outliers. Her rejoin does not pass
// against the current state, which holds her ban, so she stays banned. Ivan
// leaves twice from the same join, and the later leave enters the state.
func TestRepairStaleMemberships(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	carol := test.NewUser(t)
	dave := test.NewUser(t)
	helen := test.NewUser(t)
	ivan := test.NewUser(t)
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
		require.Eventually(t, func() bool {
			_, repairPending, cerr := db.StateKeyRepairCursor(ctx)
			require.NoError(t, cerr)
			_, recheckPending, cerr := db.RejectedRecheckCursor(ctx)
			require.NoError(t, cerr)
			return !repairPending && !recheckPending
		}, 10*time.Second, 10*time.Millisecond)

		join := map[string]any{"membership": spec.Join}
		// The events are a minute apart, as on a real server.
		at := time.Now().Add(-time.Hour)
		tick := func() time.Time {
			at = at.Add(time.Minute)
			return at
		}
		room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, bob, spec.MRoomMember, join, test.WithStateKey(bob.ID), test.WithTimestamp(tick()))
		room.CreateAndInsert(t, carol, spec.MRoomMember, join, test.WithStateKey(carol.ID), test.WithTimestamp(tick()))
		room.CreateAndInsert(t, dave, spec.MRoomMember, join, test.WithStateKey(dave.ID), test.WithTimestamp(tick()))
		room.CreateAndInsert(t, dave, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(dave.ID), test.WithTimestamp(tick()))
		room.CreateAndInsert(t, helen, spec.MRoomMember, join, test.WithStateKey(helen.ID), test.WithTimestamp(tick()))
		ivanJoin := room.CreateAndInsert(t, ivan, spec.MRoomMember, join, test.WithStateKey(ivan.ID), test.WithTimestamp(tick()))
		helenBan := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Ban}, test.WithStateKey(helen.ID), test.WithTimestamp(tick()))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))

		// The memberships that follow arrive as outliers. Their prev events
		// are known, as for events fetched as auth events of later ones.
		prev := test.WithPrevEvents(helenBan.EventID())
		bobLeave := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(bob.ID), prev, test.WithTimestamp(tick()))
		carolBan := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Ban}, test.WithStateKey(carol.ID), prev, test.WithTimestamp(tick()))
		daveProfile := room.CreateEvent(t, dave, spec.MRoomMember, join, test.WithStateKey(dave.ID), prev, test.WithTimestamp(tick()))
		room.InsertEvent(t, daveProfile)
		daveRejoin := room.CreateAndInsert(t, dave, spec.MRoomMember, map[string]any{"membership": spec.Join, "displayname": "Dave"}, test.WithStateKey(dave.ID), prev, test.WithTimestamp(tick()))
		helenUnban := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(helen.ID), prev, test.WithTimestamp(tick()))
		helenRejoin := room.CreateAndInsert(t, helen, spec.MRoomMember, join, test.WithStateKey(helen.ID), prev, test.WithTimestamp(tick()))
		ivanLeave := room.CreateEvent(t, ivan, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(ivan.ID), prev, test.WithTimestamp(tick()))
		ivanLeaveAgain := room.CreateEvent(t, ivan, spec.MRoomMember, map[string]any{"membership": spec.Leave, "reason": "again"}, test.WithStateKey(ivan.ID), prev, test.WithTimestamp(tick()))
		room.InsertEvent(t, ivanLeave)
		room.InsertEvent(t, ivanLeaveAgain)
		require.Contains(t, ivanLeaveAgain.AuthEventIDs(), ivanJoin.EventID())
		require.Contains(t, daveRejoin.AuthEventIDs(), daveProfile.EventID())
		outliers := []*types.HeaderedEvent{bobLeave, carolBan, daveProfile, daveRejoin, helenUnban, helenRejoin, ivanLeave, ivanLeaveAgain}
		for _, ev := range outliers {
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindOutlier, []*types.HeaderedEvent{ev}, "test", "test", "test", nil, false))
		}

		membership := func(user *test.User) string {
			t.Helper()
			ev, merr := db.GetStateEvent(ctx, room.ID, spec.MRoomMember, user.ID)
			require.NoError(t, merr)
			require.NotNil(t, ev)
			return gjsonMembership(t, ev)
		}
		require.Equal(t, spec.Join, membership(bob))
		require.Equal(t, spec.Join, membership(carol))
		require.Equal(t, spec.Leave, membership(dave))
		require.Equal(t, spec.Ban, membership(helen))

		sqlDB, _, err := cm.Connection(&cfg.RoomServer.Database)
		require.NoError(t, err)
		tx, err := sqlDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		if dbType == test.DBTypePostgres {
			require.NoError(t, pgdeltas.UpRejectedRecheckStaleMemberships(ctx, tx))
		} else {
			require.NoError(t, sqlitedeltas.UpRejectedRecheckStaleMemberships(ctx, tx))
		}
		require.NoError(t, tx.Commit())

		outputs, err := nc.SubscribeSync(cfg.Global.JetStream.Prefixed(jetstream.OutputRoomEvent))
		require.NoError(t, err)
		defer outputs.Unsubscribe() //nolint:errcheck

		internalAPI.RecheckRejectedEvents(ctx)
		_, pending, err := db.RejectedRecheckCursor(ctx)
		require.NoError(t, err)
		require.False(t, pending, "the recheck must finish")

		require.Equal(t, spec.Leave, membership(bob), "bob's leave must enter the state")
		require.Equal(t, spec.Ban, membership(carol), "carol's ban must enter the state")
		require.Equal(t, spec.Join, membership(dave), "dave's rejoin must enter the state")
		daveNow, err := db.GetStateEvent(ctx, room.ID, spec.MRoomMember, dave.ID)
		require.NoError(t, err)
		require.Equal(t, daveRejoin.EventID(), daveNow.EventID(), "the latest of dave's memberships must enter the state")
		require.Equal(t, spec.Ban, membership(helen), "helen's rejoin must not pass against her ban")
		ivanNow, err := db.GetStateEvent(ctx, room.ID, spec.MRoomMember, ivan.ID)
		require.NoError(t, err)
		require.Equal(t, ivanLeaveAgain.EventID(), ivanNow.EventID(), "the later of ivan's two leaves must enter the state")

		added := map[string]bool{}
		for {
			msg, nextErr := outputs.NextMsg(2 * time.Second)
			if nextErr != nil {
				break
			}
			var output api.OutputEvent
			require.NoError(t, json.Unmarshal(msg.Data, &output))
			if output.Type == api.OutputTypeNewRoomEvent {
				for _, id := range output.NewRoomEvent.AddsStateEventIDs {
					added[id] = true
				}
			}
		}
		for _, ev := range []*types.HeaderedEvent{bobLeave, carolBan, daveRejoin} {
			require.True(t, added[ev.EventID()], "the state change of %s must be announced", ev.EventID())
		}
		require.False(t, added[helenRejoin.EventID()], "helen's rejoin must not be announced")

		// The members table follows the state.
		roomInfo, err := db.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		joined, err := db.GetMembershipEventNIDsForRoom(ctx, roomInfo.RoomNID, true, false)
		require.NoError(t, err)
		require.Len(t, joined, 2, "alice and dave must be the joined members")
	})
}

func gjsonMembership(t *testing.T, ev *types.HeaderedEvent) string {
	t.Helper()
	var content struct {
		Membership string `json:"membership"`
	}
	require.NoError(t, json.Unmarshal(ev.Content(), &content))
	return content.Membership
}
