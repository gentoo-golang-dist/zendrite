// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/federationapi/routing"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

const (
	authorizedViaField     = "join_authorised_via_users_server" //nolint:misspell // Matrix spec uses British spelling
	partialJoinRemote      = spec.ServerName("joiner.test")
	partialJoinRemoteKeyID = gomatrixserverlib.KeyID("ed25519:joiner")
)

// partialJoinHarness drives the make_join and send_join handlers against a
// real roomserver hosting the room, as a remote joining server would.
type partialJoinHarness struct {
	t     *testing.T
	cfg   *config.Zendrite
	rsAPI rsapi.RoomserverInternalAPI
}

func newPartialJoinHarness(t *testing.T, dbType test.DBType) *partialJoinHarness {
	t.Helper()
	cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
	t.Cleanup(closeDB)
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	natsInstance := jetstream.NATSInstance{}
	jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
	t.Cleanup(func() { jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream) })
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	cfg.FederationAPI.ServePartialStateJoins = true
	return &partialJoinHarness{t: t, cfg: cfg, rsAPI: rsAPI}
}

func (h *partialJoinHarness) send(events ...*test.Room) {
	h.t.Helper()
	for _, room := range events {
		require.NoError(h.t, rsapi.SendEvents(context.Background(), h.rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))
	}
}

func jsonOf(t *testing.T, res any) gjson.Result {
	t.Helper()
	body, err := json.Marshal(res)
	require.NoError(t, err)
	return gjson.ParseBytes(body)
}

func (h *partialJoinHarness) makeJoin(room *test.Room, joiner *test.User) (int, gjson.Result) {
	h.t.Helper()
	path := fmt.Sprintf("/_matrix/federation/v1/make_join/%s/%s", room.ID, joiner.ID)
	req := fclient.NewFederationRequest(http.MethodGet, partialJoinRemote, h.cfg.Global.ServerName, path)
	httpReq, err := req.HTTPRequest()
	require.NoError(h.t, err)
	roomID, err := spec.NewRoomID(room.ID)
	require.NoError(h.t, err)
	userID, err := spec.NewUserID(joiner.ID, true)
	require.NoError(h.t, err)
	res := routing.MakeJoin(httpReq, &req, &h.cfg.FederationAPI, h.rsAPI, *roomID, *userID, []gomatrixserverlib.RoomVersion{room.Version})
	return res.Code, jsonOf(h.t, res.JSON)
}

// join runs make_join and a v2 send_join with the given query string, and
// returns the send_join response.
func (h *partialJoinHarness) join(room *test.Room, joiner *test.User, query string) (gomatrixserverlib.PDU, int, gjson.Result) {
	h.t.Helper()
	code, body := h.makeJoin(room, joiner)
	require.Equal(h.t, http.StatusOK, code, body.Raw)
	var proto gomatrixserverlib.ProtoEvent
	require.NoError(h.t, json.Unmarshal([]byte(body.Get("event").Raw), &proto))
	joinEvent, err := gomatrixserverlib.MustGetRoomVersion(room.Version).
		NewEventBuilderFromProtoEvent(&proto).Build(time.Now(), partialJoinRemote, partialJoinRemoteKeyID, test.PrivateKeyB)
	require.NoError(h.t, err)

	path := fmt.Sprintf("/_matrix/federation/v2/send_join/%s/%s%s", room.ID, joinEvent.EventID(), query)
	req := fclient.NewFederationRequest(http.MethodPut, partialJoinRemote, h.cfg.Global.ServerName, path)
	require.NoError(h.t, req.SetContent(joinEvent))
	httpReq, err := req.HTTPRequest()
	require.NoError(h.t, err)
	roomID, err := spec.NewRoomID(room.ID)
	require.NoError(h.t, err)
	res := routing.SendJoin(httpReq, &req, &h.cfg.FederationAPI, h.rsAPI, &test.NopJSONVerifier{}, *roomID, joinEvent.EventID(), routing.SendJoinOmitMembers(httpReq))
	return joinEvent, res.Code, jsonOf(h.t, res.JSON)
}

func eventIDs(events []gjson.Result) map[string]bool {
	ids := make(map[string]bool, len(events))
	for _, ev := range events {
		// Room versions 3 and later have no event_id field; hash the event as
		// the joining server would.
		pdu, err := gomatrixserverlib.MustGetRoomVersion(gomatrixserverlib.RoomVersionV9).NewEventFromTrustedJSON([]byte(ev.Raw), false)
		if err != nil {
			panic(err)
		}
		ids[pdu.EventID()] = true
	}
	return ids
}

// stateKeys returns "type|state_key" of the given events.
func stateKeys(events []gjson.Result) []string {
	keys := make([]string, 0, len(events))
	for _, ev := range events {
		keys = append(keys, ev.Get("type").Str+"|"+ev.Get("state_key").Str)
	}
	return keys
}

// TestSendJoinPartialState checks MSC3706 partial-state send_join responses:
// which members are omitted, that the auth chain leaves out the state, and
// servers_in_room.
func TestSendJoinPartialState(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h := newPartialJoinHarness(t, dbType)
		joiner := test.NewUser(t, test.WithSigningServer(partialJoinRemote, partialJoinRemoteKeyID, test.PrivateKeyB))
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		carol := test.NewUser(t, test.WithSigningServer("carol.test", "ed25519:carol", test.PrivateKeyB))
		dave := test.NewUser(t)
		extra := []*test.User{test.NewUser(t), test.NewUser(t), test.NewUser(t)}
		frank := test.NewUser(t)

		// A room without a name, so the heroes' memberships are included.
		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		var oldPL *rstypes.HeaderedEvent
		for _, ev := range room.Events() {
			if ev.Type() == spec.MRoomPowerLevels {
				oldPL = ev
			}
		}
		require.NotNil(t, oldPL)
		frankJoin := room.CreateAndInsert(t, frank, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(frank.ID))
		frankLeave := room.CreateAndInsert(t, frank, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(frank.ID))
		bobJoin := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		carolJoin := room.CreateAndInsert(t, carol, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(carol.ID))
		daveInvite := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(dave.ID))
		var extraJoins []string
		for _, u := range extra {
			extraJoins = append(extraJoins, room.CreateAndInsert(t, u, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(u.ID)).EventID())
		}
		// A second power levels event, whose auth events include the first.
		newPL := room.CreateAndInsert(t, alice, spec.MRoomPowerLevels, map[string]any{
			"users": map[string]any{alice.ID: 100}, "state_default": 50,
		}, test.WithStateKey(""))

		// A named room, which the joiner was invited to earlier.
		named := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		named.CreateAndInsert(t, alice, spec.MRoomName, map[string]any{"name": "named"}, test.WithStateKey(""))
		named.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		joinerInvite := named.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(joiner.ID))
		named.CreateAndInsert(t, alice, spec.MRoomTopic, map[string]any{"topic": "topic"}, test.WithStateKey(""))

		h.send(room, named)

		t.Run("omit_members=true omits members except the heroes", func(t *testing.T) {
			joinEvent, code, body := h.join(room, joiner, "?omit_members=true")
			require.Equal(t, http.StatusOK, code, body.Raw)
			assert.True(t, body.Get("members_omitted").Bool())
			assert.Equal(t, []any{"carol.test", "test"}, body.Get("servers_in_room").Value())

			state := body.Get("state").Array()
			assert.ElementsMatch(t, []string{
				"m.room.create|", "m.room.power_levels|", "m.room.join_rules|", "m.room.history_visibility|",
				// The five oldest joined or invited members are the heroes.
				"m.room.member|" + alice.ID, "m.room.member|" + bob.ID, "m.room.member|" + carol.ID,
				"m.room.member|" + dave.ID, "m.room.member|" + extra[0].ID,
			}, stateKeys(state))
			stateIDs := eventIDs(state)
			assert.True(t, stateIDs[newPL.EventID()])
			assert.True(t, stateIDs[bobJoin.EventID()])
			assert.True(t, stateIDs[carolJoin.EventID()])
			assert.True(t, stateIDs[daveInvite.EventID()])
			assert.True(t, stateIDs[extraJoins[0]])

			authIDs := eventIDs(body.Get("auth_chain").Array())
			for id := range authIDs {
				assert.False(t, stateIDs[id], "auth_chain repeats state event %s", id)
			}
			// The old power levels are in the new ones' auth chain; members who
			// are neither heroes nor in an auth chain are left out.
			assert.True(t, authIDs[oldPL.EventID()], "auth_chain lacks the old power levels")
			for _, id := range append([]string{frankJoin.EventID(), frankLeave.EventID()}, extraJoins[1:]...) {
				assert.False(t, authIDs[id] || stateIDs[id], "member event %s should be omitted", id)
			}

			// The join is now part of the room.
			member := rsapi.GetStateEvent(context.Background(), h.rsAPI, room.ID, gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomMember, StateKey: joiner.ID})
			require.NotNil(t, member)
			assert.Equal(t, joinEvent.EventID(), member.EventID())
		})

		t.Run("named room omits all other members", func(t *testing.T) {
			_, code, body := h.join(named, joiner, "?org.matrix.msc3706.partial_state=true")
			require.Equal(t, http.StatusOK, code, body.Raw)
			assert.True(t, body.Get("members_omitted").Bool())
			assert.Equal(t, []any{"test"}, body.Get("servers_in_room").Value())

			state := body.Get("state").Array()
			assert.ElementsMatch(t, []string{
				"m.room.create|", "m.room.power_levels|", "m.room.join_rules|", "m.room.history_visibility|",
				"m.room.name|", "m.room.topic|",
				// The joiner's own membership before the join is an auth event of the join.
				"m.room.member|" + joiner.ID,
			}, stateKeys(state))
			stateIDs := eventIDs(state)
			assert.True(t, stateIDs[joinerInvite.EventID()])

			// Alice's membership authorizes her state events, so it is in the
			// auth chain; Bob's authorizes nothing returned.
			authKeys := stateKeys(body.Get("auth_chain").Array())
			assert.Contains(t, authKeys, "m.room.member|"+alice.ID)
			assert.NotContains(t, authKeys, "m.room.member|"+bob.ID)
			for id := range eventIDs(body.Get("auth_chain").Array()) {
				assert.False(t, stateIDs[id], "auth_chain repeats state event %s", id)
			}
		})

		t.Run("without omit_members the state is full", func(t *testing.T) {
			other := test.NewUser(t, test.WithSigningServer(partialJoinRemote, partialJoinRemoteKeyID, test.PrivateKeyB))
			_, code, body := h.join(room, other, "")
			require.Equal(t, http.StatusOK, code, body.Raw)
			assert.False(t, body.Get("members_omitted").Exists())
			assert.False(t, body.Get("servers_in_room").Exists())
			keys := stateKeys(body.Get("state").Array())
			for _, u := range append([]*test.User{alice, bob, carol, dave, frank, joiner}, extra...) {
				assert.Contains(t, keys, "m.room.member|"+u.ID)
			}
		})

		t.Run("serve_partial_state_joins off ignores omit_members", func(t *testing.T) {
			h.cfg.FederationAPI.ServePartialStateJoins = false
			defer func() { h.cfg.FederationAPI.ServePartialStateJoins = true }()
			other := test.NewUser(t, test.WithSigningServer(partialJoinRemote, partialJoinRemoteKeyID, test.PrivateKeyB))
			_, code, body := h.join(room, other, "?omit_members=true")
			require.Equal(t, http.StatusOK, code, body.Raw)
			assert.False(t, body.Get("members_omitted").Exists())
			assert.Contains(t, stateKeys(body.Get("state").Array()), "m.room.member|"+frank.ID)
		})
	})
}

// TestSendJoinPartialStateRestricted checks that a partial-state response to
// a restricted join keeps the membership of the user who authorized it, so
// that the joining server can check the join against the returned state.
func TestSendJoinPartialStateRestricted(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h := newPartialJoinHarness(t, dbType)
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		joiner := test.NewUser(t, test.WithSigningServer(partialJoinRemote, partialJoinRemoteKeyID, test.PrivateKeyB))

		// The joiner is in the allowed room.
		allowed := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetPublicChat))
		allowed.CreateAndInsert(t, joiner, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(joiner.ID))

		// A named restricted room, so no heroes are returned.
		room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, alice, spec.MRoomName, map[string]any{"name": "restricted"}, test.WithStateKey(""))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		room.CreateAndInsert(t, alice, spec.MRoomJoinRules, map[string]any{
			"join_rule": spec.Restricted,
			"allow":     []any{map[string]any{"type": spec.MRoomMembership, "room_id": allowed.ID}},
		}, test.WithStateKey(""))
		h.send(allowed, room)

		joinEvent, code, body := h.join(room, joiner, "?omit_members=true")
		require.Equal(t, http.StatusOK, code, body.Raw)
		require.True(t, body.Get("members_omitted").Bool())
		assert.NotEmpty(t, gjson.GetBytes(joinEvent.Content(), authorizedViaField).Str)

		keys := stateKeys(body.Get("state").Array())
		assert.Contains(t, keys, "m.room.member|"+gjson.GetBytes(joinEvent.Content(), authorizedViaField).Str)
		assert.NotContains(t, keys, "m.room.member|"+bob.ID)
		// Every auth event of the join is in the state or the auth chain.
		returned := eventIDs(append(body.Get("state").Array(), body.Get("auth_chain").Array()...))
		for _, id := range joinEvent.AuthEventIDs() {
			assert.True(t, returned[id], "auth event %s of the join is missing", id)
		}
	})
}

// TestFederationMembershipRejectedDuringPartialState checks that make_join,
// send_join, make_knock, send_knock and send_leave answer 404 M_NOT_FOUND
// while this server only has partial state of the room, as Synapse does:
// it could give neither the full state nor the servers in the room.
func TestFederationMembershipRejectedDuringPartialState(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		h := newPartialJoinHarness(t, dbType)
		alice := test.NewUser(t)
		joiner := test.NewUser(t, test.WithSigningServer(partialJoinRemote, partialJoinRemoteKeyID, test.PrivateKeyB))
		room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, alice, spec.MRoomJoinRules, map[string]any{"join_rule": "knock"}, test.WithStateKey(""))
		h.send(room)

		roomID, err := spec.NewRoomID(room.ID)
		require.NoError(t, err)
		userID, err := spec.NewUserID(joiner.ID, true)
		require.NoError(t, err)

		fedCfg := &h.cfg.FederationAPI
		keys := &test.NopJSONVerifier{}
		versions := []gomatrixserverlib.RoomVersion{room.Version}
		request := func(method, path string) (fclient.FederationRequest, *http.Request) {
			req := fclient.NewFederationRequest(method, partialJoinRemote, h.cfg.Global.ServerName, path)
			if method == http.MethodPut {
				require.NoError(t, req.SetContent(map[string]any{}))
			}
			httpReq, reqErr := req.HTTPRequest()
			require.NoError(t, reqErr)
			return req, httpReq
		}
		makeKnock := func() util.JSONResponse {
			req, httpReq := request(http.MethodGet, "/_matrix/federation/v1/make_knock/"+room.ID+"/"+joiner.ID)
			return routing.MakeKnock(httpReq, &req, fedCfg, h.rsAPI, *roomID, *userID, versions)
		}

		// Before the room is partial-state, make_knock works.
		res := makeKnock()
		require.Equal(t, http.StatusOK, res.Code, jsonOf(t, res.JSON).Raw)

		roomNID, err := h.rsAPI.AssignRoomNID(context.Background(), *roomID, room.Version)
		require.NoError(t, err)
		require.NoError(t, h.rsAPI.SetRoomPartialState(context.Background(), roomNID, 0, string(partialJoinRemote), []string{string(partialJoinRemote)}, 0))

		for name, call := range map[string]func() util.JSONResponse{
			"make_join": func() util.JSONResponse {
				req, httpReq := request(http.MethodGet, "/_matrix/federation/v1/make_join/"+room.ID+"/"+joiner.ID)
				return routing.MakeJoin(httpReq, &req, fedCfg, h.rsAPI, *roomID, *userID, versions)
			},
			"send_join": func() util.JSONResponse {
				req, httpReq := request(http.MethodPut, "/_matrix/federation/v2/send_join/"+room.ID+"/$x?omit_members=true")
				return routing.SendJoin(httpReq, &req, fedCfg, h.rsAPI, keys, *roomID, "$x", true)
			},
			"make_knock": makeKnock,
			"send_knock": func() util.JSONResponse {
				req, httpReq := request(http.MethodPut, "/_matrix/federation/v1/send_knock/"+room.ID+"/$x")
				return routing.SendKnock(httpReq, &req, fedCfg, h.rsAPI, keys, *roomID, "$x")
			},
			"send_leave": func() util.JSONResponse {
				req, httpReq := request(http.MethodPut, "/_matrix/federation/v2/send_leave/"+room.ID+"/$x")
				return routing.SendLeave(httpReq, &req, fedCfg, h.rsAPI, keys, room.ID, "$x")
			},
		} {
			t.Run(name, func(t *testing.T) {
				res := call()
				body := jsonOf(t, res.JSON)
				assert.Equal(t, http.StatusNotFound, res.Code, body.Raw)
				assert.Equal(t, string(spec.ErrorNotFound), body.Get("errcode").Str, body.Raw)
			})
		}
	})
}

func TestSendJoinOmitMembers(t *testing.T) {
	for query, want := range map[string]bool{
		"":                                       false,
		"?omit_members=false":                    false,
		"?omit_members=true":                     true,
		"?org.matrix.msc3706.partial_state=true": true,
		"?org.matrix.msc3706.partial_state=1":    false,
	} {
		httpReq, err := http.NewRequest(http.MethodPut, "/_matrix/federation/v2/send_join/!r:test/$e"+query, nil)
		require.NoError(t, err)
		assert.Equal(t, want, routing.SendJoinOmitMembers(httpReq), query)
	}
}
