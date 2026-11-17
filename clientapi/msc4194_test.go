// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package clientapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
)

const msc4194Prefix = "/_matrix/client/unstable/org.matrix.msc4194/rooms/"

type msc4194Server struct {
	routers httputil.Routers
	rsAPI   rsapi.RoomserverInternalAPI
}

// redact calls POST /rooms/{roomID}/redact/user/{userID} with the raw query
// string and body.
func (s *msc4194Server) redact(t *testing.T, roomID, userID, query, body, token string) (int, gjson.Result) {
	t.Helper()
	path := msc4194Prefix + url.PathEscape(roomID) + "/redact/user/" + url.PathEscape(userID)
	if query != "" {
		path += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if body != "" {
		req = test.NewRequest(t, http.MethodPost, path, test.WithJSONBody(t, gjson.Parse(body).Value()))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.routers.Client.ServeHTTP(rec, req)
	return rec.Code, gjson.ParseBytes(rec.Body.Bytes())
}

func newMSC4194Server(t *testing.T, dbType test.DBType, mscs []string, users map[*test.User]userDevice) (*msc4194Server, context.Context) {
	t.Helper()
	cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
	t.Cleanup(closeDB)
	cfg.ClientAPI.RateLimiting.Enabled = false
	cfg.MSCs.MSCs = mscs
	routers := httputil.NewRouters()
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	natsInstance := jetstream.NATSInstance{}
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
	rsAPI.SetUserAPI(userAPI)
	AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, nil, caching.DisableMetrics)
	createAccessTokens(t, users, userAPI, processCtx.Context(), routers)
	return &msc4194Server{routers: routers, rsAPI: rsAPI}, processCtx.Context()
}

// TestRedactUserEventsMSC4194 covers
// POST /rooms/{roomId}/redact/user/{userId} (MSC4194).
func TestRedactUserEventsMSC4194(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	dave := test.NewUser(t)
	spammer := remoteUser(t, "remote.example")

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		users := map[*test.User]userDevice{alice: {}, bob: {}, dave: {}}
		s, ctx := newMSC4194Server(t, dbType, []string{"msc4194"}, users)

		join := map[string]any{"membership": spec.Join}
		message := func(n int) map[string]any {
			return map[string]any{"msgtype": "m.text", "body": fmt.Sprintf("message %d", n)}
		}
		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, bob, spec.MRoomMember, join, test.WithStateKey(bob.ID))
		room.CreateAndInsert(t, spammer, spec.MRoomMember, join, test.WithStateKey(spammer.ID))
		var spam []string
		for i := range 5 {
			content := message(i)
			if i == 2 {
				// PostgreSQL's jsonb rejects a NUL character, which event
				// JSON may contain.
				content["body"] = "nul \x00"
			}
			spam = append(spam, room.CreateAndInsert(t, spammer, "m.room.message", content).EventID())
		}
		// Sent while the spammer was joined, but received after the ban, so
		// it is soft failed.
		late := room.CreateEvent(t, spammer, "m.room.message", message(5))
		room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Ban}, test.WithStateKey(spammer.ID))
		aliceMsg := room.CreateAndInsert(t, alice, "m.room.message", message(6)).EventID()
		bobMsg := room.CreateAndInsert(t, bob, "m.room.message", message(7)).EventID()
		require.NoError(t, rsapi.SendEvents(ctx, s.rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))
		_ = rsapi.SendEvents(ctx, s.rsAPI, rsapi.KindNew, []*types.HeaderedEvent{late}, "test", "remote.example", "", nil, false)
		latest := rsapi.QueryLatestEventsAndStateResponse{}
		require.NoError(t, s.rsAPI.QueryLatestEventsAndState(ctx, &rsapi.QueryLatestEventsAndStateRequest{RoomID: room.ID}, &latest))
		require.NotContains(t, latest.LatestEvents, late.EventID(), "the late message must be soft failed")

		redacted := func(eventID string) bool {
			t.Helper()
			res := rsapi.QueryEventsByIDResponse{}
			require.NoError(t, s.rsAPI.QueryEventsByID(ctx, &rsapi.QueryEventsByIDRequest{RoomID: room.ID, EventIDs: []string{eventID}}, &res))
			require.Len(t, res.Events, 1)
			return gjson.GetBytes(res.Events[0].JSON(), "content.body").Str == ""
		}
		token := func(u *test.User) string { return users[u].accessToken }

		t.Run("access control", func(t *testing.T) {
			code, res := s.redact(t, room.ID, spammer.ID, "", "", "")
			require.Equal(t, http.StatusUnauthorized, code, res.Raw)

			// Bob lacks the redact power level and dave is not in the room.
			code, res = s.redact(t, room.ID, spammer.ID, "", "", token(bob))
			require.Equal(t, http.StatusForbidden, code, res.Raw)
			require.Equal(t, "M_FORBIDDEN", res.Get("errcode").Str)
			code, res = s.redact(t, room.ID, spammer.ID, "", "", token(dave))
			require.Equal(t, http.StatusForbidden, code, res.Raw)
			require.Equal(t, "M_FORBIDDEN", res.Get("errcode").Str)
			require.False(t, redacted(spam[0]))
		})

		t.Run("parameter validation", func(t *testing.T) {
			for name, tc := range map[string]struct{ roomID, userID, query string }{
				"invalid room ID": {"not-a-room", spammer.ID, ""},
				"invalid user ID": {room.ID, "not-a-user", ""},
				"zero limit":      {room.ID, spammer.ID, "limit=0"},
				"negative limit":  {room.ID, spammer.ID, "limit=-1"},
				"invalid limit":   {room.ID, spammer.ID, "limit=many"},
			} {
				t.Run(name, func(t *testing.T) {
					code, res := s.redact(t, tc.roomID, tc.userID, tc.query, "", token(alice))
					require.Equal(t, http.StatusBadRequest, code, res.Raw)
					require.Equal(t, "M_INVALID_PARAM", res.Get("errcode").Str)
				})
			}
			code, res := s.redact(t, room.ID, spammer.ID, "", `["not an object"]`, token(alice))
			require.Equal(t, http.StatusBadRequest, code, res.Raw)
		})

		t.Run("user who sent nothing", func(t *testing.T) {
			code, res := s.redact(t, room.ID, "@nobody:remote.example", "", "", token(alice))
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.JSONEq(t, `{"is_more_events":false,"redacted_events":{"total":0,"soft_failed":0}}`, res.Raw)
		})

		t.Run("moderator redacts in pages", func(t *testing.T) {
			// Newest first: the soft-failed message, then the last message.
			code, res := s.redact(t, room.ID, spammer.ID, "limit=2", `{"reason":"spam"}`, token(alice))
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.JSONEq(t, `{"is_more_events":true,"redacted_events":{"total":2,"soft_failed":1}}`, res.Raw)
			require.True(t, redacted(late.EventID()))
			require.True(t, redacted(spam[4]))
			require.False(t, redacted(spam[3]))

			// The four other messages and the spammer's join.
			code, res = s.redact(t, room.ID, spammer.ID, "", "", token(alice))
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.JSONEq(t, `{"is_more_events":false,"redacted_events":{"total":5,"soft_failed":0}}`, res.Raw)
			for _, id := range spam {
				require.True(t, redacted(id), id)
			}

			// Redacted events are skipped.
			code, res = s.redact(t, room.ID, spammer.ID, "limit=1000", "", token(alice))
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.JSONEq(t, `{"is_more_events":false,"redacted_events":{"total":0,"soft_failed":0}}`, res.Raw)

			require.False(t, redacted(aliceMsg), "other users' events are kept")
			require.False(t, redacted(bobMsg), "other users' events are kept")
		})

		t.Run("users redact their own events", func(t *testing.T) {
			// Bob's join and message.
			code, res := s.redact(t, room.ID, bob.ID, "", "", token(bob))
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.JSONEq(t, `{"is_more_events":false,"redacted_events":{"total":2,"soft_failed":0}}`, res.Raw)
			require.True(t, redacted(bobMsg))
			require.False(t, redacted(aliceMsg))
		})
	})
}

// TestRedactUserEventsMSC4194Disabled checks that the endpoint is not served
// without the MSC.
func TestRedactUserEventsMSC4194Disabled(t *testing.T) {
	alice := test.NewUser(t)
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		users := map[*test.User]userDevice{alice: {}}
		s, _ := newMSC4194Server(t, dbType, nil, users)
		code, res := s.redact(t, "!room:test", alice.ID, "", "", users[alice].accessToken)
		require.Equal(t, http.StatusNotFound, code, res.Raw)
		require.Equal(t, "M_UNRECOGNIZED", res.Get("errcode").Str)
	})
}
