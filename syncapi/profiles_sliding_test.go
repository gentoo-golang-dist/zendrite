// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package syncapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	appserviceAPI "codefloe.com/pat-s/zendrite/appservice/api"
	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
	"codefloe.com/pat-s/zendrite/userapi/producers"
)

// profilesUserAPI serves local users' profiles from memory, as the user API
// would from its profile tables.
type profilesUserAPI struct {
	accountDataUserAPI
	mu       sync.Mutex
	profiles map[string]*authtypes.Profile
}

func (p *profilesUserAPI) QueryProfile(_ context.Context, userID string) (*authtypes.Profile, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	profile, ok := p.profiles[userID]
	if !ok {
		return nil, appserviceAPI.ErrProfileNotExists
	}
	copied := *profile
	return &copied, nil
}

type profilesEnv struct {
	*stickyEnv
	userAPI  *profilesUserAPI
	producer *producers.SyncAPI
}

func newProfilesEnv(t *testing.T, dbType test.DBType, users ...*test.User) *profilesEnv {
	t.Helper()
	cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
	t.Cleanup(closeRig)
	cfg.ClientAPI.RateLimiting = config.RateLimiting{Enabled: false}
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	natsInstance := jetstream.NATSInstance{}
	jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
	t.Cleanup(func() { jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream) })

	env := &stickyEnv{t: t, routers: httputil.NewRouters(), devices: make(map[string]userapi.Device)}
	accounts := make([]userapi.Device, 0, len(users))
	for i, user := range users {
		dev := userapi.Device{
			ID:          fmt.Sprintf("DEVICE%d", i),
			UserID:      user.ID,
			AccessToken: fmt.Sprintf("TOKEN%d", i),
			AccountType: userapi.AccountTypeUser,
		}
		env.devices[user.ID] = dev
		accounts = append(accounts, dev)
	}
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	env.rsAPI = rsAPI
	userAPI := &profilesUserAPI{
		accountDataUserAPI: accountDataUserAPI{&syncUserAPI{accounts: accounts}},
		profiles:           make(map[string]*authtypes.Profile),
	}
	AddPublicRoutes(processCtx, env.routers, cfg, cm, &natsInstance, userAPI, rsAPI, caches, caching.DisableMetrics, nil, nil)

	db, err := storage.NewSyncServerDatasource(processCtx.Context(), cm, &cfg.SyncAPI.Database)
	require.NoError(t, err)
	env.db = db

	producer := producers.NewSyncAPI(nil, jsctx, "", "")
	producer.ProfileUpdateTopic = cfg.Global.JetStream.Prefixed(jetstream.OutputProfileUpdate)
	return &profilesEnv{stickyEnv: env, userAPI: userAPI, producer: producer}
}

// setProfile stores a local user's profile and, as the user API does,
// publishes the names of the fields that changed.
func (e *profilesEnv) setProfile(user *test.User, profile authtypes.Profile, changed ...string) {
	e.t.Helper()
	e.userAPI.mu.Lock()
	e.userAPI.profiles[user.ID] = &profile
	e.userAPI.mu.Unlock()
	if len(changed) > 0 {
		require.NoError(e.t, e.producer.SendProfileUpdate(user.ID, changed...))
	}
}

// slidingSyncRaw makes a sliding sync request with the given body.
func (e *profilesEnv) slidingSyncRaw(user *test.User, pos string, body map[string]any) gjson.Result {
	e.t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(e.t, err)
	params := map[string]string{"access_token": e.devices[user.ID].AccessToken, "timeout": "0"}
	if pos != "" {
		params["pos"] = pos
	}
	w := httptest.NewRecorder()
	e.routers.Client.ServeHTTP(w, test.NewRequest(e.t, "POST", "/_matrix/client/unstable/org.matrix.simplified_msc3575/sync",
		test.WithQueryParams(params), test.WithJSONBody(e.t, json.RawMessage(encoded))))
	require.Equal(e.t, 200, w.Code, w.Body.String())
	return gjson.Parse(w.Body.String())
}

// syncUntil repeats a sliding sync from pos until cond holds.
func (e *profilesEnv) syncUntil(user *test.User, pos string, body map[string]any, cond func(gjson.Result) bool) gjson.Result {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := e.slidingSyncRaw(user, pos, body)
		if cond(res) {
			return res
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for a sliding sync response: %s", res.Raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func profilesExt(res gjson.Result) gjson.Result {
	return res.Get(`extensions.org\.matrix\.msc4262\.profiles`)
}

func profileOf(res gjson.Result, userID string) gjson.Result {
	return profilesExt(res).Get("users." + gjson.Escape(userID))
}

func profilesBody(lists map[string]any, subscriptions map[string]any, ext map[string]any) map[string]any {
	body := map[string]any{"extensions": map[string]any{"org.matrix.msc4262.profiles": ext}}
	if lists != nil {
		body["lists"] = lists
	}
	if subscriptions != nil {
		body["room_subscriptions"] = subscriptions
	}
	return body
}

var allRoomsList = map[string]any{"all": map[string]any{"ranges": [][]int{{0, 20}}, "required_state": []any{}, "timeline_limit": 1}}

func rawString(t *testing.T, s string) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(s)
	require.NoError(t, err)
	return encoded
}

// An initial sliding sync sends the whole profiles of the members of the
// rooms in the response, and later responses only the fields that changed,
// as updated or removed. A response is acknowledged only when the client
// syncs from its pos, the fields filter applies throughout, and users who
// share no room with the syncing user are left out.
func TestProfilesSlidingSync(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		carol := test.NewUser(t)
		env := newProfilesEnv(t, dbType, alice, bob, carol)
		env.setProfile(alice, authtypes.Profile{DisplayName: "Alice"})
		env.setProfile(bob, authtypes.Profile{
			DisplayName: "Bob", AvatarURL: "mxc://test/bob",
			Fields: map[string]json.RawMessage{"m.tz": rawString(t, "Europe/Berlin")},
		})
		env.setProfile(carol, authtypes.Profile{DisplayName: "Carol"})

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		join := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(bob.ID))
		env.send(room.Events()...)
		env.waitFor(alice, room.ID, join)

		body := profilesBody(allRoomsList, nil, map[string]any{"enabled": true})
		res := env.slidingSyncRaw(alice, "", body)
		require.JSONEq(t, `{"updated":{"displayname":"Alice"}}`, profileOf(res, alice.ID).Raw, res.Raw)
		require.JSONEq(t, `{"updated":{"displayname":"Bob","avatar_url":"mxc://test/bob","m.tz":"Europe/Berlin"}}`, profileOf(res, bob.ID).Raw, res.Raw)
		require.False(t, profileOf(res, carol.ID).Exists(), res.Raw)
		pos := res.Get("pos").Str

		res = env.slidingSyncRaw(alice, pos, body)
		require.False(t, profilesExt(res).Exists(), "nothing changed: %s", res.Raw)
		pos = res.Get("pos").Str

		// Bob changes his display name; carol, who shares no room with
		// alice, changes hers too.
		env.setProfile(carol, authtypes.Profile{DisplayName: "Carol 2"}, authtypes.ProfileFieldDisplayName)
		env.setProfile(bob, authtypes.Profile{
			DisplayName: "Bob 2", AvatarURL: "mxc://test/bob",
			Fields: map[string]json.RawMessage{"m.tz": rawString(t, "Europe/Berlin")},
		}, authtypes.ProfileFieldDisplayName)
		res = env.syncUntil(alice, pos, body, func(r gjson.Result) bool { return profilesExt(r).Exists() })
		require.JSONEq(t, `{"users":{"`+bob.ID+`":{"updated":{"displayname":"Bob 2"}}}}`, profilesExt(res).Raw)

		// Without the new pos, the update is not acknowledged.
		// Syncing from the old pos discards the response's pos.
		again := env.slidingSyncRaw(alice, pos, body)
		require.JSONEq(t, profilesExt(res).Raw, profilesExt(again).Raw)
		pos = again.Get("pos").Str
		res = env.slidingSyncRaw(alice, pos, body)
		require.False(t, profilesExt(res).Exists(), "acknowledged: %s", res.Raw)
		pos = res.Get("pos").Str

		// Bob removes a custom field and alice changes her own avatar.
		env.setProfile(bob, authtypes.Profile{DisplayName: "Bob 2", AvatarURL: "mxc://test/bob"}, "m.tz")
		env.setProfile(alice, authtypes.Profile{DisplayName: "Alice", AvatarURL: "mxc://test/alice"}, authtypes.ProfileFieldAvatarURL)
		res = env.syncUntil(alice, pos, body, func(r gjson.Result) bool {
			return profileOf(r, bob.ID).Exists() && profileOf(r, alice.ID).Exists()
		})
		require.JSONEq(t, `{"removed":["m.tz"]}`, profileOf(res, bob.ID).Raw, res.Raw)
		require.JSONEq(t, `{"updated":{"avatar_url":"mxc://test/alice"}}`, profileOf(res, alice.ID).Raw, res.Raw)
		pos = res.Get("pos").Str

		// The fields filter leaves out other fields, and users with none of
		// the requested fields.
		avatarOnly := profilesBody(allRoomsList, nil, map[string]any{"enabled": true, "fields": []string{"avatar_url"}})
		res = env.slidingSyncRaw(alice, "", avatarOnly)
		require.JSONEq(t, `{"users":{"`+alice.ID+`":{"updated":{"avatar_url":"mxc://test/alice"}},"`+bob.ID+`":{"updated":{"avatar_url":"mxc://test/bob"}}}}`, profilesExt(res).Raw)
		env.setProfile(bob, authtypes.Profile{DisplayName: "Bob 3", AvatarURL: "mxc://test/bob"}, authtypes.ProfileFieldDisplayName)
		env.setProfile(alice, authtypes.Profile{DisplayName: "Alice", AvatarURL: "mxc://test/alice2"}, authtypes.ProfileFieldAvatarURL)
		res = env.syncUntil(alice, pos, avatarOnly, func(r gjson.Result) bool { return profilesExt(r).Exists() })
		require.JSONEq(t, `{"users":{"`+alice.ID+`":{"updated":{"avatar_url":"mxc://test/alice2"}}}}`, profilesExt(res).Raw)
	})
}

// Users who join a shared room get their whole profile sent, remote users'
// from their membership event, which also carries their later changes, and
// users who leave the last shared room are sent as null.
func TestProfilesSlidingSyncMembership(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		carol := test.NewUser(t)
		dave := test.NewUser(t, test.WithSigningServer("remote.example", "ed25519:remote", test.PrivateKeyB))
		env := newProfilesEnv(t, dbType, alice, carol)
		env.setProfile(alice, authtypes.Profile{DisplayName: "Alice"})
		env.setProfile(carol, authtypes.Profile{DisplayName: "Carol", AvatarURL: "mxc://test/carol"})

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		env.send(room.Events()...)
		env.waitFor(alice, room.ID, room.Events()[len(room.Events())-1])

		body := profilesBody(allRoomsList, nil, map[string]any{"enabled": true})
		res := env.slidingSyncRaw(alice, "", body)
		require.JSONEq(t, `{"users":{"`+alice.ID+`":{"updated":{"displayname":"Alice"}}}}`, profilesExt(res).Raw)
		pos := res.Get("pos").Str

		carolJoin := room.CreateAndInsert(t, carol, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(carol.ID))
		daveJoin := room.CreateAndInsert(t, dave, spec.MRoomMember, map[string]any{"membership": "join", "displayname": "Dave"}, test.WithStateKey(dave.ID))
		env.send(carolJoin, daveJoin)
		env.waitFor(alice, room.ID, daveJoin)
		res = env.slidingSyncRaw(alice, pos, body)
		require.JSONEq(t, `{"updated":{"displayname":"Carol","avatar_url":"mxc://test/carol"}}`, profileOf(res, carol.ID).Raw, res.Raw)
		require.JSONEq(t, `{"updated":{"displayname":"Dave"}}`, profileOf(res, dave.ID).Raw, res.Raw)
		pos = res.Get("pos").Str

		daveRename := room.CreateAndInsert(t, dave, spec.MRoomMember, map[string]any{"membership": "join", "displayname": "Dave 2", "avatar_url": "mxc://remote.example/dave"}, test.WithStateKey(dave.ID))
		env.send(daveRename)
		env.waitFor(alice, room.ID, daveRename)
		res = env.slidingSyncRaw(alice, pos, body)
		require.JSONEq(t, `{"updated":{"displayname":"Dave 2","avatar_url":"mxc://remote.example/dave"}}`, profileOf(res, dave.ID).Raw, res.Raw)
		require.False(t, profileOf(res, carol.ID).Exists(), res.Raw)
		pos = res.Get("pos").Str

		carolLeave := room.CreateAndInsert(t, carol, spec.MRoomMember, map[string]any{"membership": "leave"}, test.WithStateKey(carol.ID))
		env.send(carolLeave)
		env.waitFor(alice, room.ID, carolLeave)
		res = env.slidingSyncRaw(alice, pos, body)
		user := profileOf(res, carol.ID)
		require.Equal(t, gjson.Null, user.Type, res.Raw)
		require.True(t, user.Exists(), res.Raw)
		require.False(t, profileOf(res, dave.ID).Exists(), res.Raw)
	})
}

// For a room whose members load lazily, only the users its response mentions
// (timeline senders, member state and, the first time, heroes) have their
// profiles sent. A room new to the connection sends its members' profiles
// once.
func TestProfilesSlidingSyncRoomSubset(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		carol := test.NewUser(t)
		others := make([]*test.User, 6)
		for i := range others {
			others[i] = test.NewUser(t)
		}
		env := newProfilesEnv(t, dbType, append([]*test.User{alice, bob, carol}, others...)...)
		env.setProfile(alice, authtypes.Profile{DisplayName: "Alice"})
		env.setProfile(bob, authtypes.Profile{DisplayName: "Bob"})
		env.setProfile(carol, authtypes.Profile{DisplayName: "Carol"})

		lazyRoom := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		for i, other := range others {
			env.setProfile(other, authtypes.Profile{DisplayName: fmt.Sprintf("Other %d", i)})
			lazyRoom.CreateAndInsert(t, other, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(other.ID))
		}
		lazyRoom.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(bob.ID))
		msg := lazyRoom.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "hi", "msgtype": "m.text"})
		env.send(lazyRoom.Events()...)
		env.waitFor(alice, lazyRoom.ID, msg)

		otherRoom := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		otherJoin := otherRoom.CreateAndInsert(t, carol, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(carol.ID))
		env.send(otherRoom.Events()...)
		env.waitFor(alice, otherRoom.ID, otherJoin)

		lazy := map[string]any{"timeline_limit": 1, "required_state": [][]string{{spec.MRoomMember, "$LAZY"}}}
		body := profilesBody(nil, map[string]any{lazyRoom.ID: lazy}, map[string]any{"enabled": true})
		res := env.slidingSyncRaw(alice, "", body)
		want := map[string]bool{alice.ID: true, bob.ID: true}
		for _, hero := range res.Get("rooms." + gjson.Escape(lazyRoom.ID) + ".heroes.#.user_id").Array() {
			want[hero.Str] = true
		}
		got := map[string]bool{}
		profilesExt(res).Get("users").ForEach(func(key, _ gjson.Result) bool {
			got[key.Str] = true
			return true
		})
		require.Equal(t, want, got, res.Raw)
		require.Less(t, len(got), 2+len(others), "lazy loading leaves members out: %s", res.Raw)
		pos := res.Get("pos").Str

		// The other room enters the subset: its members' profiles follow.
		full := map[string]any{"timeline_limit": 1, "required_state": []any{}}
		body = profilesBody(nil, map[string]any{lazyRoom.ID: lazy, otherRoom.ID: full}, map[string]any{"enabled": true})
		res = env.slidingSyncRaw(alice, pos, body)
		require.True(t, res.Get("rooms."+gjson.Escape(otherRoom.ID)).Exists(), res.Raw)
		require.JSONEq(t, `{"updated":{"displayname":"Carol"}}`, profileOf(res, carol.ID).Raw, res.Raw)
		require.JSONEq(t, `{"updated":{"displayname":"Alice"}}`, profileOf(res, alice.ID).Raw, res.Raw)
		pos = res.Get("pos").Str

		// Once sent, the room's members are not sent again.
		msg = otherRoom.CreateAndInsert(t, carol, "m.room.message", map[string]any{"body": "hi", "msgtype": "m.text"})
		env.send(msg)
		env.waitFor(alice, otherRoom.ID, msg)
		res = env.slidingSyncRaw(alice, pos, body)
		require.True(t, res.Get("rooms."+gjson.Escape(otherRoom.ID)).Exists(), res.Raw)
		require.False(t, profilesExt(res).Exists(), res.Raw)
	})
}

// Membership changes in a room the syncing user left are not theirs to see:
// a remote user who renames there is not sent, although they still share
// another room, and a user who joins it after the leave is not sent as null.
func TestProfilesSlidingSyncLeftRoom(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		carol := test.NewUser(t)
		dave := test.NewUser(t, test.WithSigningServer("remote.example", "ed25519:remote", test.PrivateKeyB))
		env := newProfilesEnv(t, dbType, alice, bob, carol)
		env.setProfile(alice, authtypes.Profile{DisplayName: "Alice"})

		kept := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		keptJoin := kept.CreateAndInsert(t, dave, spec.MRoomMember, map[string]any{"membership": "join", "displayname": "Dave"}, test.WithStateKey(dave.ID))
		env.send(kept.Events()...)
		env.waitFor(alice, kept.ID, keptJoin)

		left := test.NewRoom(t, bob, test.RoomPreset(test.PresetPublicChat))
		left.CreateAndInsert(t, dave, spec.MRoomMember, map[string]any{"membership": "join", "displayname": "Dave"}, test.WithStateKey(dave.ID))
		aliceJoin := left.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(alice.ID))
		env.send(left.Events()...)
		env.waitFor(alice, left.ID, aliceJoin)

		body := profilesBody(allRoomsList, nil, map[string]any{"enabled": true})
		pos := env.slidingSyncRaw(alice, "", body).Get("pos").Str

		aliceLeave := left.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": "leave"}, test.WithStateKey(alice.ID))
		carolJoin := left.CreateAndInsert(t, carol, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(carol.ID))
		daveRename := left.CreateAndInsert(t, dave, spec.MRoomMember, map[string]any{"membership": "join", "displayname": "Dave 2"}, test.WithStateKey(dave.ID))
		marker := kept.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "marker", "msgtype": "m.text"})
		env.send(aliceLeave, carolJoin, daveRename)
		env.send(marker)
		env.waitFor(alice, kept.ID, marker)

		res := env.slidingSyncRaw(alice, pos, body)
		require.False(t, profileOf(res, dave.ID).Exists(), res.Raw)
		require.False(t, profileOf(res, carol.ID).Exists(), res.Raw)
		require.True(t, profileOf(res, bob.ID).Exists(), "bob shares no room any more: %s", res.Raw)
		require.Equal(t, gjson.Null, profileOf(res, bob.ID).Type, res.Raw)
	})
}
