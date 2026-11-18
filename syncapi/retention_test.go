// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package syncapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
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
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// TestRetentionPolicies checks MSC1763: non-state events past the room's
// effective max_lifetime are no longer served by /sync, /messages, /event
// or /context, while state events and newer events are.
func TestRetentionPolicies(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		aliceDev := api.Device{ID: "ALICEID", UserID: alice.ID, AccessToken: "ALICE_MSC1763_TOKEN", AccountType: api.AccountTypeUser}

		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cfg.Global.Retention = config.Retention{
			Enabled:            true,
			DefaultPolicy:      config.RetentionPolicy{MaxLifetime: 24 * time.Hour},
			AllowedLifetimeMin: 30 * time.Minute,
		}
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		AddPublicRoutes(processCtx, routers, cfg, cm, &natsInstance, &syncUserAPI{accounts: []api.Device{aliceDev}}, rsAPI, caches, caching.DisableMetrics, nil, nil)

		now := time.Now()
		message := func(room *test.Room, body string, age time.Duration) string {
			return room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": body}, test.WithTimestamp(now.Add(-age))).EventID()
		}

		// A room whose policy keeps events for an hour.
		hourRoom := test.NewRoom(t, alice)
		hourRoom.CreateAndInsert(t, alice, "m.room.retention", map[string]any{"max_lifetime": time.Hour.Milliseconds()}, test.WithStateKey(""))
		hourExpired := message(hourRoom, "two hours old", 2*time.Hour)
		hourTopic := hourRoom.CreateAndInsert(t, alice, spec.MRoomTopic, map[string]any{"topic": "old topic"}, test.WithStateKey(""), test.WithTimestamp(now.Add(-2*time.Hour))).EventID()
		hourKept := message(hourRoom, "ten minutes old", 10*time.Minute)

		// A room whose policy asks for a minute, raised to 30 minutes by
		// allowed_lifetime_min.
		minuteRoom := test.NewRoom(t, alice)
		minuteRoom.CreateAndInsert(t, alice, "m.room.retention", map[string]any{"max_lifetime": time.Minute.Milliseconds()}, test.WithStateKey(""))
		minuteExpired := message(minuteRoom, "an hour old", time.Hour)
		minuteKept := message(minuteRoom, "ten minutes old", 10*time.Minute)

		// A room without a policy gets the default one.
		defaultRoom := test.NewRoom(t, alice)
		defaultExpired := message(defaultRoom, "two days old", 48*time.Hour)
		defaultKept := message(defaultRoom, "two hours old", 2*time.Hour)

		for _, room := range []*test.Room{hourRoom, minuteRoom, defaultRoom} {
			require.NoError(t, rsapi.SendEvents(context.Background(), rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))
		}
		last := defaultRoom.Events()[len(defaultRoom.Events())-1].EventID()
		syncUntil(t, routers, aliceDev.AccessToken, false, func(syncBody string) bool {
			return gjson.Get(syncBody, fmt.Sprintf(`rooms.join.%s.timeline.events.#(event_id=="%s")`, defaultRoom.ID, last)).Exists()
		})

		cases := []struct {
			room             *test.Room
			expired, visible []string
		}{
			{hourRoom, []string{hourExpired}, []string{hourKept, hourTopic}},
			{minuteRoom, []string{minuteExpired}, []string{minuteKept}},
			{defaultRoom, []string{defaultExpired}, []string{defaultKept}},
		}

		w := httptest.NewRecorder()
		routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/sync", test.WithQueryParams(map[string]string{
			"access_token": aliceDev.AccessToken,
			"filter":       `{"room":{"timeline":{"limit":50}}}`,
		})))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		syncBody := w.Body.String()

		for _, c := range cases {
			timeline := gjson.Get(syncBody, fmt.Sprintf("rooms.join.%s.timeline.events", c.room.ID))
			w = httptest.NewRecorder()
			routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages", c.room.ID), test.WithQueryParams(map[string]string{
				"access_token": aliceDev.AccessToken,
				"dir":          "b",
				"limit":        "50",
			})))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			chunk := gjson.Get(w.Body.String(), "chunk")

			for _, eventID := range c.expired {
				assert.False(t, timeline.Get(fmt.Sprintf(`#(event_id=="%s")`, eventID)).Exists(), "/sync served expired %s", eventID)
				assert.False(t, chunk.Get(fmt.Sprintf(`#(event_id=="%s")`, eventID)).Exists(), "/messages served expired %s", eventID)
				w = httptest.NewRecorder()
				routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, fmt.Sprintf("/_matrix/client/v3/rooms/%s/event/%s", c.room.ID, eventID), test.WithQueryParams(map[string]string{"access_token": aliceDev.AccessToken})))
				assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
			}
			for _, eventID := range c.visible {
				assert.True(t, timeline.Get(fmt.Sprintf(`#(event_id=="%s")`, eventID)).Exists(), "/sync dropped %s", eventID)
				assert.True(t, chunk.Get(fmt.Sprintf(`#(event_id=="%s")`, eventID)).Exists(), "/messages dropped %s", eventID)
				w = httptest.NewRecorder()
				routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, fmt.Sprintf("/_matrix/client/v3/rooms/%s/event/%s", c.room.ID, eventID), test.WithQueryParams(map[string]string{"access_token": aliceDev.AccessToken})))
				assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
			}
		}

		// /context around a kept event leaves the expired one out.
		w = httptest.NewRecorder()
		routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, fmt.Sprintf("/_matrix/client/v3/rooms/%s/context/%s", hourRoom.ID, hourKept), test.WithQueryParams(map[string]string{
			"access_token": aliceDev.AccessToken,
			"limit":        "50",
		})))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.False(t, gjson.Get(w.Body.String(), fmt.Sprintf(`events_before.#(event_id=="%s")`, hourExpired)).Exists(), w.Body.String())
		assert.True(t, gjson.Get(w.Body.String(), fmt.Sprintf(`events_before.#(event_id=="%s")`, hourTopic)).Exists(), w.Body.String())
	})
}
