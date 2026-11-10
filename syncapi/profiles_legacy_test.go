// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package syncapi

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	"codefloe.com/pat-s/zendrite/test"
)

// profileFieldsFilter is a /sync filter that opts in to MSC4429 profile
// updates for the given fields, with extra merged into it.
func profileFieldsFilter(t *testing.T, ids []string, extra map[string]any) string {
	t.Helper()
	filter := map[string]any{
		"room":                              map[string]any{"timeline": map[string]any{"limit": 5}},
		"org.matrix.msc4429.profile_fields": map[string]any{"ids": ids},
	}
	for key, value := range extra {
		filter[key] = value
	}
	encoded, err := json.Marshal(filter)
	require.NoError(t, err)
	return string(encoded)
}

func legacyProfiles(res gjson.Result) gjson.Result {
	return res.Get(`org\.matrix\.msc4429\.users`)
}

func legacyProfileOf(res gjson.Result, userID string) gjson.Result {
	return legacyProfiles(res).Get(gjson.Escape(userID) + ".profile_updates")
}

// legacySyncUntil repeats a /sync from since until cond holds.
func (e *profilesEnv) legacySyncUntil(user *test.User, since, filter string, cond func(gjson.Result) bool) gjson.Result {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res := e.sync(user, since, filter)
		if cond(res) {
			return res
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for a /sync response: %s", res.Raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// hasTimelineEvent reports whether a /sync response carries the event in
// the room's timeline.
func hasTimelineEvent(roomID, eventID string) func(gjson.Result) bool {
	return func(res gjson.Result) bool {
		return res.Get(fmt.Sprintf(`rooms.join.%s.timeline.events.#(event_id==%q)`, gjson.Escape(roomID), eventID)).Exists()
	}
}

// syncWithTimeout makes a long-polling /sync and returns its response and
// how long it took.
func (e *profilesEnv) syncWithTimeout(user *test.User, since, filter string, timeout time.Duration) (gjson.Result, time.Duration) {
	params := map[string]string{
		"access_token": e.devices[user.ID].AccessToken,
		"timeout":      fmt.Sprint(timeout.Milliseconds()),
		"since":        since,
		"filter":       filter,
	}
	w := httptest.NewRecorder()
	start := time.Now()
	e.routers.Client.ServeHTTP(w, test.NewRequest(e.t, "GET", "/_matrix/client/v3/sync", test.WithQueryParams(params)))
	took := time.Since(start)
	if w.Code != 200 {
		return gjson.Result{}, took
	}
	return gjson.Parse(w.Body.String()), took
}

// Only a filter with profile fields opts in. An initial /sync then sends the
// requested fields of the syncing user and of the members of its rooms, and
// incremental ones the requested fields that changed, null for removed ones.
// Users who share no room with the syncing user are left out.
func TestProfilesLegacySync(t *testing.T) {
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

		// Without the filter field, nothing is sent.
		res := env.sync(alice, "", `{"room":{"timeline":{"limit":5}}}`)
		require.False(t, legacyProfiles(res).Exists(), res.Raw)
		plainSince := res.Get("next_batch").Str

		filter := profileFieldsFilter(t, []string{"displayname", "avatar_url", "m.tz"}, nil)
		res = env.sync(alice, "", filter)
		require.JSONEq(t, `{
			"`+alice.ID+`":{"profile_updates":{"displayname":"Alice"}},
			"`+bob.ID+`":{"profile_updates":{"displayname":"Bob","avatar_url":"mxc://test/bob","m.tz":"Europe/Berlin"}}
		}`, legacyProfiles(res).Raw, res.Raw)
		since := res.Get("next_batch").Str

		// The ids select the fields; users with none of them are left out.
		res = env.sync(alice, "", profileFieldsFilter(t, []string{"m.tz"}, nil))
		require.JSONEq(t, `{"`+bob.ID+`":{"profile_updates":{"m.tz":"Europe/Berlin"}}}`, legacyProfiles(res).Raw, res.Raw)
		res = env.sync(alice, "", profileFieldsFilter(t, []string{}, nil))
		require.False(t, legacyProfiles(res).Exists(), res.Raw)

		res = env.sync(alice, since, filter)
		require.False(t, legacyProfiles(res).Exists(), "nothing changed: %s", res.Raw)

		// Bob changes his display name; carol, who shares no room with
		// alice, changes hers too.
		env.setProfile(carol, authtypes.Profile{DisplayName: "Carol 2"}, authtypes.ProfileFieldDisplayName)
		env.setProfile(bob, authtypes.Profile{
			DisplayName: "Bob 2", AvatarURL: "mxc://test/bob",
			Fields: map[string]json.RawMessage{"m.tz": rawString(t, "Europe/Berlin")},
		}, authtypes.ProfileFieldDisplayName)
		res = env.legacySyncUntil(alice, since, filter, func(r gjson.Result) bool { return legacyProfiles(r).Exists() })
		require.JSONEq(t, `{"`+bob.ID+`":{"profile_updates":{"displayname":"Bob 2"}}}`, legacyProfiles(res).Raw, res.Raw)
		since = res.Get("next_batch").Str

		res = env.sync(alice, since, filter)
		require.False(t, legacyProfiles(res).Exists(), "already sent: %s", res.Raw)

		// Bob removes a custom field and alice changes her own avatar.
		env.setProfile(bob, authtypes.Profile{DisplayName: "Bob 2", AvatarURL: "mxc://test/bob"}, "m.tz")
		env.setProfile(alice, authtypes.Profile{DisplayName: "Alice", AvatarURL: "mxc://test/alice"}, authtypes.ProfileFieldAvatarURL)
		res = env.legacySyncUntil(alice, since, filter, func(r gjson.Result) bool {
			return legacyProfileOf(r, bob.ID).Exists() && legacyProfileOf(r, alice.ID).Exists()
		})
		require.JSONEq(t, `{"m.tz":null}`, legacyProfileOf(res, bob.ID).Raw, res.Raw)
		require.JSONEq(t, `{"avatar_url":"mxc://test/alice"}`, legacyProfileOf(res, alice.ID).Raw, res.Raw)

		// A client that did not opt in gets none of these changes.
		res = env.sync(alice, plainSince, `{"room":{"timeline":{"limit":5}}}`)
		require.False(t, legacyProfiles(res).Exists(), res.Raw)

		// Changes to fields the client did not ask for are not sent.
		env.setProfile(bob, authtypes.Profile{DisplayName: "Bob 3", AvatarURL: "mxc://test/bob"}, authtypes.ProfileFieldDisplayName)
		env.setProfile(alice, authtypes.Profile{DisplayName: "Alice", AvatarURL: "mxc://test/alice2"}, authtypes.ProfileFieldAvatarURL)
		avatarOnly := profileFieldsFilter(t, []string{"avatar_url"}, nil)
		res = env.legacySyncUntil(alice, since, avatarOnly, func(r gjson.Result) bool { return legacyProfileOf(r, alice.ID).Exists() })
		require.JSONEq(t, `{"`+alice.ID+`":{"profile_updates":{"avatar_url":"mxc://test/alice2"}}}`, legacyProfiles(res).Raw, res.Raw)
	})
}

// A stored filter keeps the profile fields, so that a filter ID opts in too.
func TestProfilesLegacySyncStoredFilter(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		env := newProfilesEnv(t, dbType, alice)
		env.setProfile(alice, authtypes.Profile{DisplayName: "Alice"})

		w := httptest.NewRecorder()
		env.routers.Client.ServeHTTP(w, test.NewRequest(t, "POST", "/_matrix/client/v3/user/"+alice.ID+"/filter",
			test.WithQueryParams(map[string]string{"access_token": env.devices[alice.ID].AccessToken}),
			test.WithJSONBody(t, json.RawMessage(profileFieldsFilter(t, []string{"displayname"}, nil)))))
		require.Equal(t, 200, w.Code, w.Body.String())
		filterID := gjson.Get(w.Body.String(), "filter_id").Str
		require.NotEmpty(t, filterID)

		w = httptest.NewRecorder()
		env.routers.Client.ServeHTTP(w, test.NewRequest(t, "GET", "/_matrix/client/v3/user/"+alice.ID+"/filter/"+filterID,
			test.WithQueryParams(map[string]string{"access_token": env.devices[alice.ID].AccessToken})))
		require.Equal(t, 200, w.Code, w.Body.String())
		require.JSONEq(t, `{"ids":["displayname"]}`, gjson.Get(w.Body.String(), `org\.matrix\.msc4429\.profile_fields`).Raw, w.Body.String())

		res := env.sync(alice, "", filterID)
		require.JSONEq(t, `{"`+alice.ID+`":{"profile_updates":{"displayname":"Alice"}}}`, legacyProfiles(res).Raw, res.Raw)
	})
}

// Users who join a shared room get their requested fields sent, remote users'
// from their membership event, which also carries their later changes, and
// users who leave the last shared room are sent with profile_updates null.
func TestProfilesLegacySyncMembership(t *testing.T) {
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

		filter := profileFieldsFilter(t, []string{"displayname", "avatar_url"}, nil)
		res := env.sync(alice, "", filter)
		require.JSONEq(t, `{"`+alice.ID+`":{"profile_updates":{"displayname":"Alice"}}}`, legacyProfiles(res).Raw, res.Raw)
		since := res.Get("next_batch").Str

		carolJoin := room.CreateAndInsert(t, carol, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(carol.ID))
		daveJoin := room.CreateAndInsert(t, dave, spec.MRoomMember, map[string]any{"membership": "join", "displayname": "Dave"}, test.WithStateKey(dave.ID))
		env.send(carolJoin, daveJoin)
		env.waitFor(alice, room.ID, daveJoin)
		res = env.sync(alice, since, filter)
		require.JSONEq(t, `{"displayname":"Carol","avatar_url":"mxc://test/carol"}`, legacyProfileOf(res, carol.ID).Raw, res.Raw)
		require.JSONEq(t, `{"displayname":"Dave"}`, legacyProfileOf(res, dave.ID).Raw, res.Raw)
		since = res.Get("next_batch").Str

		daveRename := room.CreateAndInsert(t, dave, spec.MRoomMember, map[string]any{"membership": "join", "displayname": "Dave 2", "avatar_url": "mxc://remote.example/dave"}, test.WithStateKey(dave.ID))
		env.send(daveRename)
		env.waitFor(alice, room.ID, daveRename)
		res = env.sync(alice, since, filter)
		require.JSONEq(t, `{"displayname":"Dave 2","avatar_url":"mxc://remote.example/dave"}`, legacyProfileOf(res, dave.ID).Raw, res.Raw)
		require.False(t, legacyProfileOf(res, carol.ID).Exists(), res.Raw)
		since = res.Get("next_batch").Str

		carolLeave := room.CreateAndInsert(t, carol, spec.MRoomMember, map[string]any{"membership": "leave"}, test.WithStateKey(carol.ID))
		env.send(carolLeave)
		env.waitFor(alice, room.ID, carolLeave)
		res = env.sync(alice, since, filter)
		user := legacyProfileOf(res, carol.ID)
		require.True(t, user.Exists(), res.Raw)
		require.Equal(t, gjson.Null, user.Type, res.Raw)
		require.False(t, legacyProfileOf(res, dave.ID).Exists(), res.Raw)
	})
}

// With lazy-loaded members, only the members the response loads have their
// profiles sent, and a member's profile follows their membership event: it
// is sent again only when a later response loads the member again. An
// initial sync, which resets which members a device has loaded, would load
// them again, so the test waits for events with incremental syncs.
func TestProfilesLegacySyncLazyMembers(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		others := make([]*test.User, 4)
		for i := range others {
			others[i] = test.NewUser(t)
		}
		env := newProfilesEnv(t, dbType, append([]*test.User{alice, bob}, others...)...)
		env.setProfile(alice, authtypes.Profile{DisplayName: "Alice"})
		env.setProfile(bob, authtypes.Profile{DisplayName: "Bob"})

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		for i, other := range others {
			env.setProfile(other, authtypes.Profile{DisplayName: fmt.Sprintf("Other %d", i)})
			room.CreateAndInsert(t, other, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(other.ID))
		}
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(bob.ID))
		msg := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "hi", "msgtype": "m.text"})
		env.send(room.Events()...)
		env.waitFor(alice, room.ID, msg)

		filter := profileFieldsFilter(t, []string{"displayname"}, map[string]any{
			"room": map[string]any{
				"timeline": map[string]any{"limit": 1},
				"state":    map[string]any{"lazy_load_members": true},
			},
		})
		res := env.sync(alice, "", filter)
		got := map[string]bool{}
		legacyProfiles(res).ForEach(func(key, _ gjson.Result) bool {
			got[key.Str] = true
			return true
		})
		require.Equal(t, map[string]bool{alice.ID: true, bob.ID: true}, got, res.Raw)
		since := res.Get("next_batch").Str

		// Bob talks again: his membership was already loaded, so neither it
		// nor his profile is sent again.
		msg = room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "again", "msgtype": "m.text"})
		env.send(msg)
		res = env.legacySyncUntil(alice, since, filter, hasTimelineEvent(room.ID, msg.EventID()))
		require.False(t, res.Get("rooms.join."+gjson.Escape(room.ID)+".state").Exists(), res.Raw)
		require.False(t, legacyProfiles(res).Exists(), res.Raw)
		since = res.Get("next_batch").Str

		// Another member talks for the first time: their profile follows.
		msg = room.CreateAndInsert(t, others[0], "m.room.message", map[string]any{"body": "hello", "msgtype": "m.text"})
		env.send(msg)
		res = env.legacySyncUntil(alice, since, filter, hasTimelineEvent(room.ID, msg.EventID()))
		require.JSONEq(t, `{"`+others[0].ID+`":{"profile_updates":{"displayname":"Other 0"}}}`, legacyProfiles(res).Raw, res.Raw)
	})
}

// A long poll that opted in returns when a user it shares a room with
// changes a requested field; one that did not opt in keeps waiting.
func TestProfilesLegacySyncLongPoll(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		env := newProfilesEnv(t, dbType, alice, bob)
		env.setProfile(alice, authtypes.Profile{DisplayName: "Alice"})
		env.setProfile(bob, authtypes.Profile{DisplayName: "Bob"})

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		join := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(bob.ID))
		env.send(room.Events()...)
		env.waitFor(alice, room.ID, join)

		plain := `{"room":{"timeline":{"limit":5}}}`
		filter := profileFieldsFilter(t, []string{"displayname"}, nil)
		plainSince := env.sync(alice, "", plain).Get("next_batch").Str
		since := env.sync(alice, "", filter).Get("next_batch").Str

		type result struct {
			res  gjson.Result
			took time.Duration
		}
		optedIn := make(chan result, 1)
		notOptedIn := make(chan result, 1)
		const plainTimeout = 3 * time.Second
		go func() {
			res, took := env.syncWithTimeout(alice, since, filter, 20*time.Second)
			optedIn <- result{res, took}
		}()
		go func() {
			res, took := env.syncWithTimeout(alice, plainSince, plain, plainTimeout)
			notOptedIn <- result{res, took}
		}()
		// Let both requests start waiting.
		time.Sleep(500 * time.Millisecond)
		env.setProfile(bob, authtypes.Profile{DisplayName: "Bob 2"}, authtypes.ProfileFieldDisplayName)

		got := <-optedIn
		require.Less(t, got.took, 15*time.Second, got.res.Raw)
		require.JSONEq(t, `{"`+bob.ID+`":{"profile_updates":{"displayname":"Bob 2"}}}`, legacyProfiles(got.res).Raw, got.res.Raw)

		got = <-notOptedIn
		require.GreaterOrEqual(t, got.took, plainTimeout-100*time.Millisecond, got.res.Raw)
		require.False(t, legacyProfiles(got.res).Exists(), got.res.Raw)
		require.False(t, got.res.Get("rooms").Exists(), got.res.Raw)
	})
}

// When the syncing user joins a room, the members its state carries have
// their requested fields sent.
func TestProfilesLegacySyncJoinedRoom(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		eve := test.NewUser(t)
		env := newProfilesEnv(t, dbType, alice, eve)
		env.setProfile(alice, authtypes.Profile{DisplayName: "Alice"})
		env.setProfile(eve, authtypes.Profile{
			DisplayName: "Eve",
			Fields:      map[string]json.RawMessage{"m.tz": rawString(t, "Europe/Vienna")},
		})

		home := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		env.send(home.Events()...)
		env.waitFor(alice, home.ID, home.Events()[len(home.Events())-1])

		filter := profileFieldsFilter(t, []string{"m.tz"}, nil)
		res := env.sync(alice, "", filter)
		require.False(t, legacyProfiles(res).Exists(), res.Raw)
		since := res.Get("next_batch").Str

		room := test.NewRoom(t, eve, test.RoomPreset(test.PresetPublicChat))
		for i := range 10 {
			room.CreateAndInsert(t, eve, "m.room.message", map[string]any{"body": fmt.Sprint(i), "msgtype": "m.text"})
		}
		join := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(alice.ID))
		env.send(room.Events()...)
		res = env.legacySyncUntil(alice, since, filter, hasTimelineEvent(room.ID, join.EventID()))
		require.JSONEq(t, `{"`+eve.ID+`":{"profile_updates":{"m.tz":"Europe/Vienna"}}}`, legacyProfiles(res).Raw, res.Raw)
	})
}
