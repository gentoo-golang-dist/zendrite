// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	fsAPI "codefloe.com/pat-s/zendrite/federationapi/api"
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

// remoteStateFederationAPI answers /state_ids with a fixed state per server
// and records the servers asked.
type remoteStateFederationAPI struct {
	fsAPI.RoomserverFederationAPI
	state map[spec.ServerName][]string
	asked *sync.Map
}

func (remoteStateFederationAPI) IsServerBackingOff(spec.ServerName) bool { return false }

func (f remoteStateFederationAPI) LookupStateIDs(
	_ context.Context, _, server spec.ServerName, _, _ string,
) (gomatrixserverlib.StateIDResponse, error) {
	f.asked.Store(server, true)
	return fclient.RespStateIDs{StateEventIDs: f.state[server]}, nil
}

// Where the local repair cannot add a later membership stored as an outlier,
// because it does not pass against the current state or because the
// memberships fork, the room's state on the servers of its moderators
// decides. A membership is taken only where they all agree on it, and only if
// it follows the one the current state holds. Other servers are not asked.
//
// Moderators on a.test and b.test may kick and ban. Mallory's server c.test
// is in the room too, and would report Frank's rejoin.
//
// Helen was banned, and her unban and rejoin arrive as outliers. Both servers
// hold her rejoin, so it enters the state. Frank's history is the same, but
// the servers disagree, so he stays banned. Kim's too, but both servers
// report her join from before the ban, which does not follow it, so she stays
// banned. Gina's leave and a profile change both follow her join; both
// servers hold the leave.
func TestRepairMembershipsFromRemoteState(t *testing.T) {
	alice := test.NewUser(t)
	helen := test.NewUser(t)
	frank := test.NewUser(t)
	kim := test.NewUser(t)
	gina := test.NewUser(t)
	modA := test.NewUser(t, test.WithSigningServer("a.test", "ed25519:a", test.PrivateKeyA))
	modB := test.NewUser(t, test.WithSigningServer("b.test", "ed25519:b", test.PrivateKeyB))
	mallory := test.NewUser(t, test.WithSigningServer("c.test", "ed25519:c", test.PrivateKeyA))
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
			_, pending, cerr := db.RejectedRecheckCursor(ctx)
			require.NoError(t, cerr)
			return !pending
		}, 10*time.Second, 10*time.Millisecond)

		at := time.Now().Add(-time.Hour)
		tick := func() time.Time {
			at = at.Add(time.Minute)
			return at
		}
		join := map[string]any{"membership": spec.Join}
		leave := map[string]any{"membership": spec.Leave}
		ban := map[string]any{"membership": spec.Ban}
		room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetPublicChat))
		for _, user := range []*test.User{modA, modB, mallory} {
			room.CreateAndInsert(t, user, spec.MRoomMember, join, test.WithStateKey(user.ID), test.WithTimestamp(tick()))
		}
		var powerLevels map[string]any
		for _, ev := range room.CurrentState() {
			if ev.Type() == spec.MRoomPowerLevels {
				require.NoError(t, json.Unmarshal(ev.Content(), &powerLevels))
			}
		}
		users, _ := powerLevels["users"].(map[string]any)
		users[modA.ID], users[modB.ID], users[mallory.ID] = 50, 50, 0
		room.CreateAndInsert(t, alice, spec.MRoomPowerLevels, powerLevels, test.WithStateKey(""), test.WithTimestamp(tick()))
		joins := map[string]*types.HeaderedEvent{}
		for _, user := range []*test.User{helen, frank, kim, gina} {
			joins[user.ID] = room.CreateAndInsert(t, user, spec.MRoomMember, join, test.WithStateKey(user.ID), test.WithTimestamp(tick()))
		}
		bans := map[string]*types.HeaderedEvent{}
		for _, user := range []*test.User{helen, frank, kim} {
			bans[user.ID] = room.CreateAndInsert(t, alice, spec.MRoomMember, ban, test.WithStateKey(user.ID), test.WithTimestamp(tick()))
		}
		last := bans[kim.ID]
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))

		// The memberships that follow arrive as outliers.
		prev := test.WithPrevEvents(last.EventID())
		var outliers []*types.HeaderedEvent
		rejoins := map[string]*types.HeaderedEvent{}
		for _, user := range []*test.User{helen, frank, kim} {
			outliers = append(outliers, room.CreateAndInsert(t, alice, spec.MRoomMember, leave, test.WithStateKey(user.ID), prev, test.WithTimestamp(tick())))
			rejoins[user.ID] = room.CreateAndInsert(t, user, spec.MRoomMember, join, test.WithStateKey(user.ID), prev, test.WithTimestamp(tick()))
			outliers = append(outliers, rejoins[user.ID])
		}
		ginaLeave := room.CreateEvent(t, gina, spec.MRoomMember, leave, test.WithStateKey(gina.ID), prev, test.WithTimestamp(tick()))
		ginaProfile := room.CreateEvent(t, gina, spec.MRoomMember, map[string]any{"membership": spec.Join, "displayname": "Gina"},
			test.WithStateKey(gina.ID), prev, test.WithTimestamp(tick()))
		room.InsertEvent(t, ginaLeave)
		room.InsertEvent(t, ginaProfile)
		require.Contains(t, ginaLeave.AuthEventIDs(), joins[gina.ID].EventID())
		require.Contains(t, ginaProfile.AuthEventIDs(), joins[gina.ID].EventID())
		outliers = append(outliers, ginaLeave, ginaProfile)
		for _, ev := range outliers {
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindOutlier, []*types.HeaderedEvent{ev}, "test", "test", "test", nil, false))
		}

		// Each server's state, with the given membership events in place of
		// the room's.
		stateWith := func(members map[string]*types.HeaderedEvent) []string {
			var ids []string
			for _, ev := range room.CurrentState() {
				if ev.Type() == spec.MRoomMember {
					if replaced, found := members[*ev.StateKey()]; found {
						ids = append(ids, replaced.EventID())
						continue
					}
				}
				ids = append(ids, ev.EventID())
			}
			return ids
		}
		common := map[string]*types.HeaderedEvent{
			helen.ID: rejoins[helen.ID],
			kim.ID:   joins[kim.ID],
			gina.ID:  ginaLeave,
		}
		serverA := map[string]*types.HeaderedEvent{frank.ID: rejoins[frank.ID]}
		serverB := map[string]*types.HeaderedEvent{frank.ID: bans[frank.ID]}
		for k, v := range common {
			serverA[k], serverB[k] = v, v
		}
		serverC := map[string]*types.HeaderedEvent{frank.ID: rejoins[frank.ID]}
		for k, v := range common {
			serverC[k] = v
		}
		asked := &sync.Map{}
		rsAPI.SetFederationAPI(remoteStateFederationAPI{
			state: map[spec.ServerName][]string{"a.test": stateWith(serverA), "b.test": stateWith(serverB), "c.test": stateWith(serverC)},
			asked: asked,
		}, nil)

		membership := func(user *test.User) string {
			t.Helper()
			ev, merr := db.GetStateEvent(ctx, room.ID, spec.MRoomMember, user.ID)
			require.NoError(t, merr)
			require.NotNil(t, ev)
			return gjsonMembership(t, ev)
		}
		require.Equal(t, spec.Ban, membership(helen))
		require.Equal(t, spec.Join, membership(gina))

		sqlDB, _, err := cm.Connection(&cfg.RoomServer.Database)
		require.NoError(t, err)
		tx, err := sqlDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		if dbType == test.DBTypePostgres {
			require.NoError(t, pgdeltas.UpRejectedRecheckRemoteMemberships(ctx, tx))
		} else {
			require.NoError(t, sqlitedeltas.UpRejectedRecheckRemoteMemberships(ctx, tx))
		}
		require.NoError(t, tx.Commit())

		internalAPI.RecheckRejectedEvents(ctx)

		helenNow, err := db.GetStateEvent(ctx, room.ID, spec.MRoomMember, helen.ID)
		require.NoError(t, err)
		require.Equal(t, rejoins[helen.ID].EventID(), helenNow.EventID(), "helen's rejoin, which both servers hold, must enter the state")
		require.Equal(t, spec.Ban, membership(frank), "frank must stay banned when the servers disagree")
		require.Equal(t, spec.Ban, membership(kim), "kim's join from before her ban must not replace it")
		ginaNow, err := db.GetStateEvent(ctx, room.ID, spec.MRoomMember, gina.ID)
		require.NoError(t, err)
		require.Equal(t, ginaLeave.EventID(), ginaNow.EventID(), "gina's leave, which both servers hold, must enter the state")
		_, askedA := asked.Load(spec.ServerName("a.test"))
		_, askedB := asked.Load(spec.ServerName("b.test"))
		_, askedC := asked.Load(spec.ServerName("c.test"))
		require.True(t, askedA && askedB, "the moderators' servers must be asked")
		require.False(t, askedC, "a server without a moderator must not be asked")
	})
}
