// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package clientapi

import (
	"context"
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
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
)

// TestRichTopicsAndHierarchySummary covers rich text topics (MSC3765) in
// createRoom, /publicRooms and /hierarchy, and the room version and
// encryption algorithm in /hierarchy (MSC3266).
func TestRichTopicsAndHierarchySummary(t *testing.T) {
	alice := test.NewUser(t)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		cfg.ClientAPI.RateLimiting.Enabled = false
		defer closeDB()
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, nil, caching.DisableMetrics)
		accessTokens := map[*test.User]userDevice{alice: {}}
		createAccessTokens(t, accessTokens, userAPI, processCtx.Context(), routers)

		do := func(method, path string, body any) gjson.Result {
			t.Helper()
			opts := []test.HTTPRequestOpt{}
			if body != nil {
				opts = append(opts, test.WithJSONBody(t, body))
			}
			req := test.NewRequest(t, method, path, opts...)
			req.Header.Set("Authorization", "Bearer "+accessTokens[alice].accessToken)
			rec := httptest.NewRecorder()
			routers.Client.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			return gjson.ParseBytes(rec.Body.Bytes())
		}

		// The child room's topic has an HTML representation first, so the
		// plain text one must be picked, not the legacy topic key.
		child := do(http.MethodPost, "/_matrix/client/v3/createRoom", map[string]any{
			"preset":     "public_chat",
			"visibility": "public",
			"initial_state": []map[string]any{
				{"type": spec.MRoomEncryption, "state_key": "", "content": map[string]any{"algorithm": "m.megolm.v1.aes-sha2"}},
				{"type": spec.MRoomTopic, "state_key": "", "content": map[string]any{
					"topic": "legacy topic",
					"m.topic": map[string]any{"m.text": []map[string]any{
						{"body": "<b>Rich</b> topic", "mimetype": "text/html"},
						{"body": "Rich topic"},
					}},
				}},
			},
		}).Get("room_id").Str
		space := do(http.MethodPost, "/_matrix/client/v3/createRoom", map[string]any{
			"preset":           "public_chat",
			"visibility":       "public",
			"topic":            "Space topic",
			"creation_content": map[string]any{"type": spec.MSpace},
			"initial_state": []map[string]any{
				{"type": spec.MSpaceChild, "state_key": child, "content": map[string]any{"via": []string{string(cfg.Global.ServerName)}}},
			},
		}).Get("room_id").Str

		// createRoom's topic carries a text/plain m.topic block.
		spaceID, err := spec.NewRoomID(space)
		require.NoError(t, err)
		topicEv, err := rsAPI.CurrentStateEvent(context.Background(), *spaceID, spec.MRoomTopic, "")
		require.NoError(t, err)
		require.NotNil(t, topicEv)
		require.Equal(t, "Space topic", gjson.GetBytes(topicEv.Content(), "topic").Str)
		require.Equal(t, "Space topic", gjson.GetBytes(topicEv.Content(), `m\.topic.m\.text.0.body`).Str)
		require.Equal(t, "text/plain", gjson.GetBytes(topicEv.Content(), `m\.topic.m\.text.0.mimetype`).Str)

		topics := map[string]string{}
		for _, room := range do(http.MethodGet, "/_matrix/client/v3/publicRooms", nil).Get("chunk").Array() {
			topics[room.Get("room_id").Str] = room.Get("topic").Str
		}
		require.Equal(t, map[string]string{space: "Space topic", child: "Rich topic"}, topics)

		rooms := map[string]gjson.Result{}
		for _, room := range do(http.MethodGet, "/_matrix/client/v1/rooms/"+url.PathEscape(space)+"/hierarchy", nil).Get("rooms").Array() {
			rooms[room.Get("room_id").Str] = room
		}
		require.Len(t, rooms, 2)
		defaultVersion := string(rsAPI.DefaultRoomVersion())
		require.Equal(t, defaultVersion, rooms[space].Get("room_version").Str)
		require.False(t, rooms[space].Get("encryption").Exists(), rooms[space].Raw)
		require.Equal(t, "Space topic", rooms[space].Get("topic").Str)
		require.Equal(t, defaultVersion, rooms[child].Get("room_version").Str)
		require.Equal(t, "m.megolm.v1.aes-sha2", rooms[child].Get("encryption").Str)
		require.Equal(t, "Rich topic", rooms[child].Get("topic").Str)
	})
}
