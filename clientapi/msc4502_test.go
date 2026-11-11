// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package clientapi

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
	uapi "codefloe.com/pat-s/zendrite/userapi/api"
)

const msc4502Prefix = "/_matrix/client/unstable/io.element.msc4502/rooms/"

// msc4502Server is a client API with a room that local and remote users
// joined and left, and two appservices: one granted the MSC4502 scope and
// one without it.
type msc4502Server struct {
	routers httputil.Routers
	roomID  string
}

// get calls GET /rooms/{roomID}/is_joined with the raw query string.
func (s *msc4502Server) get(t *testing.T, roomID, query, token string) (int, gjson.Result) {
	t.Helper()
	path := msc4502Prefix + url.PathEscape(roomID) + "/is_joined"
	if query != "" {
		path += "?" + query
	}
	req := test.NewRequest(t, http.MethodGet, path)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.routers.Client.ServeHTTP(rec, req)
	return rec.Code, gjson.ParseBytes(rec.Body.Bytes())
}

// newMSC4502Server starts the client API with the given MSCs enabled after
// mutate has adjusted the configuration. Room is sent to the roomserver and
// users gets access tokens through password login (without MSC3861 only).
func newMSC4502Server(
	t *testing.T, dbType test.DBType, mscs []string, mutate func(*config.Zendrite),
	room *test.Room, users map[*test.User]userDevice,
) *msc4502Server {
	t.Helper()
	cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
	t.Cleanup(closeDB)
	cfg.ClientAPI.RateLimiting.Enabled = false
	cfg.MSCs.MSCs = mscs
	server := string(cfg.Global.ServerName)
	for _, as := range []struct{ id string }{{"scoped"}, {"plain"}} {
		namespace := regexp.MustCompile(`@_` + as.id + `_.*:` + regexp.QuoteMeta(server))
		registration := config.ApplicationService{
			ID:              as.id,
			URL:             "null",
			ASToken:         "as_" + as.id,
			HSToken:         "hs_" + as.id,
			SenderLocalpart: "_" + as.id + "_bot",
			NamespaceMap: map[string][]config.ApplicationServiceNamespace{
				"users": {{Exclusive: true, Regex: namespace.String(), RegexpObject: namespace}},
			},
		}
		if as.id == "scoped" {
			registration.Scopes = []string{config.ScopeQueryRoomMembership}
		}
		cfg.Derived.ApplicationServices = append(cfg.Derived.ApplicationServices, registration)
	}
	if mutate != nil {
		mutate(cfg)
	}

	routers := httputil.NewRouters()
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	natsInstance := jetstream.NATSInstance{}
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
	rsAPI.SetUserAPI(userAPI)
	AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, nil, caching.DisableMetrics)

	ctx := processCtx.Context()
	if len(users) > 0 {
		createAccessTokens(t, users, userAPI, ctx, routers)
	}
	// The appservices' senders, as the appservice component sets them up.
	for _, id := range []string{"scoped", "plain"} {
		localpart := "_" + id + "_bot"
		require.NoError(t, userAPI.PerformAccountCreation(ctx, &uapi.PerformAccountCreationRequest{
			AccountType:  uapi.AccountTypeAppService,
			Localpart:    localpart,
			ServerName:   cfg.Global.ServerName,
			AppServiceID: id,
			OnConflict:   uapi.ConflictUpdate,
		}, &uapi.PerformAccountCreationResponse{}))
		require.NoError(t, userAPI.PerformDeviceCreation(ctx, &uapi.PerformDeviceCreationRequest{
			Localpart:          localpart,
			ServerName:         cfg.Global.ServerName,
			AccessToken:        "as_" + id,
			DeviceID:           &localpart,
			NoDeviceListUpdate: true,
		}, &uapi.PerformDeviceCreationResponse{}))
	}
	require.NoError(t, rsapi.SendEvents(ctx, rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))
	return &msc4502Server{routers: routers, roomID: room.ID}
}

// msc4502Room builds a room alice created, which bob and a user of
// remote.example joined, and which charlie and a user of gone.example joined
// and left again.
func msc4502Room(t *testing.T, alice, bob, charlie, remote, gone *test.User) *test.Room {
	t.Helper()
	room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
	join := map[string]any{"membership": spec.Join}
	leave := map[string]any{"membership": spec.Leave}
	for _, u := range []*test.User{bob, charlie, remote, gone} {
		room.CreateAndInsert(t, u, spec.MRoomMember, join, test.WithStateKey(u.ID))
	}
	for _, u := range []*test.User{charlie, gone} {
		room.CreateAndInsert(t, u, spec.MRoomMember, leave, test.WithStateKey(u.ID))
	}
	return room
}

func remoteUser(t *testing.T, server spec.ServerName) *test.User {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return test.NewUser(t, test.WithSigningServer(server, "ed25519:remote", key))
}

// TestRoomIsJoinedMSC4502 covers GET /rooms/{roomId}/is_joined (MSC4502).
func TestRoomIsJoinedMSC4502(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	charlie := test.NewUser(t)
	dave := test.NewUser(t)
	admin := test.NewUser(t, test.WithAccountType(uapi.AccountTypeAdmin))
	remote := remoteUser(t, "remote.example")
	gone := remoteUser(t, "gone.example")

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		users := map[*test.User]userDevice{alice: {}, bob: {}, dave: {}, admin: {}}
		room := msc4502Room(t, alice, bob, charlie, remote, gone)
		s := newMSC4502Server(t, dbType, []string{"msc4502"}, nil, room, users)
		const scoped = "as_scoped"
		mxid := func(u *test.User) string { return "mxid=" + url.QueryEscape(u.ID) }

		t.Run("access control", func(t *testing.T) {
			code, res := s.get(t, s.roomID, mxid(bob), "")
			require.Equal(t, http.StatusUnauthorized, code, res.Raw)
			require.Equal(t, "M_MISSING_TOKEN", res.Get("errcode").Str)

			// Room members and server admins lack the scope.
			for _, u := range []*test.User{alice, bob, admin, dave} {
				code, res = s.get(t, s.roomID, mxid(bob), users[u].accessToken)
				require.Equal(t, http.StatusForbidden, code, res.Raw)
				require.Equal(t, "M_FORBIDDEN", res.Get("errcode").Str)
			}
			code, res = s.get(t, s.roomID, mxid(bob), "as_plain")
			require.Equal(t, http.StatusForbidden, code, res.Raw)
			require.Equal(t, "M_FORBIDDEN", res.Get("errcode").Str)
			code, res = s.get(t, s.roomID, mxid(bob)+"&user_id="+url.QueryEscape("@_plain_bot:test"), "as_plain")
			require.Equal(t, http.StatusForbidden, code, res.Raw)

			// The scope is checked before the parameters.
			code, res = s.get(t, "not-a-room-id", "", "as_plain")
			require.Equal(t, http.StatusForbidden, code, res.Raw)

			code, res = s.get(t, s.roomID, mxid(bob), scoped)
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.JSONEq(t, `{"joined":true}`, res.Raw)
			// The appservice keeps its scope while masquerading as one of
			// its users.
			code, res = s.get(t, s.roomID, mxid(bob)+"&user_id="+url.QueryEscape("@_scoped_bot:test"), scoped)
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.JSONEq(t, `{"joined":true}`, res.Raw)
		})

		t.Run("parameter validation", func(t *testing.T) {
			for name, tc := range map[string]struct {
				roomID, query, errcode string
			}{
				"neither parameter":   {s.roomID, "", "M_MISSING_PARAM"},
				"both parameters":     {s.roomID, mxid(bob) + "&server_name=test", "M_MISSING_PARAM"},
				"both, mxid invalid":  {s.roomID, "mxid=nope&server_name=test", "M_MISSING_PARAM"},
				"invalid room ID":     {"not-a-room-id", mxid(bob), "M_INVALID_PARAM"},
				"invalid mxid":        {s.roomID, "mxid=not-a-userid", "M_INVALID_PARAM"},
				"empty mxid":          {s.roomID, "mxid=", "M_INVALID_PARAM"},
				"mxid without domain": {s.roomID, "mxid=%40bob", "M_INVALID_PARAM"},
				"invalid server name": {s.roomID, "server_name=foo_bar", "M_INVALID_PARAM"},
				"empty server name":   {s.roomID, "server_name=", "M_INVALID_PARAM"},
			} {
				t.Run(name, func(t *testing.T) {
					code, res := s.get(t, tc.roomID, tc.query, scoped)
					require.Equal(t, http.StatusBadRequest, code, res.Raw)
					require.Equal(t, tc.errcode, res.Get("errcode").Str)
				})
			}
		})

		t.Run("membership", func(t *testing.T) {
			for name, tc := range map[string]struct {
				roomID, query string
				joined        bool
			}{
				"creator":                       {s.roomID, mxid(alice), true},
				"local user joined":             {s.roomID, mxid(bob), true},
				"local user left":               {s.roomID, mxid(charlie), false},
				"local user never in room":      {s.roomID, mxid(dave), false},
				"unknown local user":            {s.roomID, "mxid=" + url.QueryEscape("@nobody:test"), false},
				"remote user joined":            {s.roomID, mxid(remote), true},
				"remote user left":              {s.roomID, mxid(gone), false},
				"unknown remote user":           {s.roomID, "mxid=" + url.QueryEscape("@nobody:remote.example"), false},
				"local server":                  {s.roomID, "server_name=test", true},
				"remote server joined":          {s.roomID, "server_name=remote.example", true},
				"remote server whose user left": {s.roomID, "server_name=gone.example", false},
				"unknown server":                {s.roomID, "server_name=unknown.org", false},
				"server name with port":         {s.roomID, "server_name=remote.example%3A8448", false},
				"unknown room, user":            {"!unknown:unknown.org", mxid(bob), false},
				"unknown room, server":          {"!unknown:unknown.org", "server_name=unknown.org", false},
				"unknown room, local server":    {"!unknown:test", "server_name=test", false},
			} {
				t.Run(name, func(t *testing.T) {
					code, res := s.get(t, tc.roomID, tc.query, scoped)
					require.Equal(t, http.StatusOK, code, res.Raw)
					var body map[string]any
					require.NoError(t, json.Unmarshal([]byte(res.Raw), &body))
					require.Equal(t, map[string]any{"joined": tc.joined}, body)
				})
			}
		})
	})
}

// TestRoomIsJoinedMSC4502Disabled checks that the endpoint is only served
// with the MSC enabled, as in Synapse.
func TestRoomIsJoinedMSC4502Disabled(t *testing.T) {
	alice := test.NewUser(t)
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		room := test.NewRoom(t, alice)
		s := newMSC4502Server(t, dbType, nil, nil, room, nil)
		code, res := s.get(t, s.roomID, "mxid="+url.QueryEscape(alice.ID), "as_scoped")
		require.Equal(t, http.StatusNotFound, code, res.Raw)
		require.Equal(t, "M_UNRECOGNIZED", res.Get("errcode").Str)
	})
}

// TestRoomIsJoinedMSC4502OAuth covers the scope token under delegated
// authentication (MSC3861): the access token's own scope grants access, and
// appservices still get it from their registration.
func TestRoomIsJoinedMSC4502OAuth(t *testing.T) {
	alice := test.NewUser(t)
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		// Introspection results are cached by token, so the tokens differ
		// per database.
		tokenPrefix := strings.ReplaceAll(t.Name(), "/", "_") + "_"
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, r.ParseForm())
			scope := "urn:matrix:client:api:* urn:matrix:client:device:DEVICE"
			username := "oauth_plain"
			switch strings.TrimPrefix(r.PostForm.Get("token"), tokenPrefix) {
			case "scoped":
				scope += " " + config.ScopeQueryRoomMembership
				username = "oauth_scoped"
			case "stable":
				// The stable scope token is not recognized yet, as in Synapse.
				scope += " urn:matrix:client:rooms:is_joined"
			case "admin":
				scope += " urn:synapse:admin:*"
				username = "oauth_admin"
			case "plain":
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"active": true, "sub": username, "username": username, "scope": scope,
			})
		}))
		t.Cleanup(provider.Close)

		room := test.NewRoom(t, alice)
		s := newMSC4502Server(t, dbType, []string{"msc3861", "msc4502"}, func(cfg *config.Zendrite) {
			cfg.MSCs.MSC3861.Issuer = provider.URL + "/"
			cfg.MSCs.MSC3861.ClientID = "client-id"
			cfg.MSCs.MSC3861.ClientSecret = "client-secret"
			cfg.MSCs.MSC3861.IntrospectionEndpoint = provider.URL + "/introspect"
		}, room, nil)
		query := "mxid=" + url.QueryEscape(alice.ID)

		code, res := s.get(t, s.roomID, query, tokenPrefix+"scoped")
		require.Equal(t, http.StatusOK, code, res.Raw)
		require.JSONEq(t, `{"joined":true}`, res.Raw)

		for _, token := range []string{"plain", "stable", "admin"} {
			code, res = s.get(t, s.roomID, query, tokenPrefix+token)
			require.Equal(t, http.StatusForbidden, code, token+": "+res.Raw)
			require.Equal(t, "M_FORBIDDEN", res.Get("errcode").Str)
		}

		code, res = s.get(t, s.roomID, query, "as_scoped")
		require.Equal(t, http.StatusOK, code, res.Raw)
		require.JSONEq(t, `{"joined":true}`, res.Raw)
		code, res = s.get(t, s.roomID, query, "as_plain")
		require.Equal(t, http.StatusForbidden, code, res.Raw)
	})
}
