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
	rsstorage "codefloe.com/pat-s/zendrite/roomserver/storage"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	syncshared "codefloe.com/pat-s/zendrite/syncapi/storage/shared"
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

// TestRetentionPurge checks the MSC1763 purge jobs: the roomserver and the
// sync API each delete expired non-state events, but keep state events, the
// room's forward extremity and its latest event.
func TestRetentionPurge(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		aliceDev := api.Device{ID: "ALICEID", UserID: alice.ID, AccessToken: "ALICE_MSC1763_PURGE_TOKEN", AccountType: api.AccountTypeUser}

		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cfg.Global.Retention = config.Retention{
			Enabled:         true,
			DefaultPolicy:   config.RetentionPolicy{MaxLifetime: time.Hour},
			PurgeJobs:       []config.RetentionPurgeJob{{Interval: 100 * time.Millisecond}},
			FirstPurgeDelay: 100 * time.Millisecond,
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
		room := test.NewRoom(t, alice)
		expired := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "expired"}, test.WithTimestamp(now.Add(-2*time.Hour))).EventID()
		oldState := room.CreateAndInsert(t, alice, spec.MRoomTopic, map[string]any{"topic": "old"}, test.WithStateKey(""), test.WithTimestamp(now.Add(-2*time.Hour))).EventID()
		recent := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "recent"}).EventID()
		last := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "expired but latest"}, test.WithTimestamp(now.Add(-2*time.Hour))).EventID()
		require.NoError(t, rsapi.SendEvents(context.Background(), rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))
		syncUntil(t, routers, aliceDev.AccessToken, false, func(syncBody string) bool {
			return gjson.Get(syncBody, fmt.Sprintf(`rooms.join.%s.timeline.events.#(event_id=="%s")`, room.ID, recent)).Exists()
		})

		syncDB, err := storage.NewSyncServerDatasource(processCtx.Context(), cm, &cfg.SyncAPI.Database)
		require.NoError(t, err)
		inRoomserver := func(eventID string) bool {
			var res rsapi.QueryEventsByIDResponse
			require.NoError(t, rsAPI.QueryEventsByID(context.Background(), &rsapi.QueryEventsByIDRequest{RoomID: room.ID, EventIDs: []string{eventID}}, &res))
			return len(res.Events) == 1
		}
		inSyncAPI := func(eventID string) bool {
			events, err := syncDB.Events(context.Background(), []string{eventID})
			require.NoError(t, err)
			return len(events) == 1
		}
		require.Eventually(t, func() bool {
			return !inRoomserver(expired) && !inSyncAPI(expired)
		}, 20*time.Second, 50*time.Millisecond, "the expired event was not purged")
		for _, eventID := range []string{oldState, recent, last} {
			assert.True(t, inRoomserver(eventID), "the roomserver purged %s", eventID)
			assert.True(t, inSyncAPI(eventID), "the sync API purged %s", eventID)
		}
	})
}

// TestRetentionPurgeBatches drives both purges one event at a time, so that
// batches made of kept events (state events, the latest events) must not
// stop them before older expired events.
func TestRetentionPurgeBatches(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		aliceDev := api.Device{ID: "ALICEID", UserID: alice.ID, AccessToken: "ALICE_MSC1763_BATCH_TOKEN", AccountType: api.AccountTypeUser}

		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
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
		room := test.NewRoom(t, alice)
		var expired, kept []string
		for i := range 3 {
			old := now.Add(-time.Duration(10-i) * time.Hour)
			expired = append(expired, room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": fmt.Sprint("old ", i)}, test.WithTimestamp(old)).EventID())
			kept = append(kept, room.CreateAndInsert(t, alice, spec.MRoomTopic, map[string]any{"topic": fmt.Sprint(i)}, test.WithStateKey(""), test.WithTimestamp(old)).EventID())
		}
		kept = append(kept, room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "latest"}, test.WithTimestamp(now.Add(-2*time.Hour))).EventID())
		require.NoError(t, rsapi.SendEvents(context.Background(), rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))
		syncUntil(t, routers, aliceDev.AccessToken, false, func(syncBody string) bool {
			return gjson.Get(syncBody, fmt.Sprintf(`rooms.join.%s.timeline.events.#(event_id=="%s")`, room.ID, kept[len(kept)-1])).Exists()
		})
		cutoff := now.Add(-time.Hour).UnixMilli()

		rsDB, err := rsstorage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches)
		require.NoError(t, err)
		var rsPurged []string
		var after rstypes.EventNID
		for {
			purged, next, done, err := rsDB.PurgeExpiredEvents(context.Background(), room.ID, cutoff, after, 1)
			require.NoError(t, err)
			rsPurged = append(rsPurged, purged...)
			if done {
				break
			}
			after = next
		}
		assert.ElementsMatch(t, expired, rsPurged)

		syncDB, err := storage.NewSyncServerDatasource(processCtx.Context(), cm, &cfg.SyncAPI.Database)
		require.NoError(t, err)
		var syncPurged []string
		cursor := syncshared.RetentionCursor{TS: cutoff - 1}
		for {
			purged, next, done, err := syncDB.PurgeExpiredEvents(context.Background(), room.ID, cursor, 1)
			require.NoError(t, err)
			syncPurged = append(syncPurged, purged...)
			if done {
				break
			}
			cursor = next
		}
		assert.ElementsMatch(t, expired, syncPurged)

		events, err := syncDB.Events(context.Background(), kept)
		require.NoError(t, err)
		assert.Len(t, events, len(kept))
		var res rsapi.QueryEventsByIDResponse
		require.NoError(t, rsAPI.QueryEventsByID(context.Background(), &rsapi.QueryEventsByIDRequest{RoomID: room.ID, EventIDs: append(kept, expired...)}, &res))
		assert.Len(t, res.Events, len(kept))
	})
}

// TestRetentionPurgeSkipsEventInFlight checks that the roomserver's purge
// leaves alone an expired event that the room's input worker has stored but
// not yet made a forward extremity, since purging it would fail the event's
// processing (MSC1763).
func TestRetentionPurgeSkipsEventInFlight(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)

		now := time.Now()
		room := test.NewRoom(t, alice)
		require.NoError(t, rsapi.SendEvents(context.Background(), rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))

		// Store an expired event the way the input worker does before it
		// locks the room, without updating the room's latest events.
		ctx := context.Background()
		rsDB, err := rsstorage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches)
		require.NoError(t, err)
		inFlight := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "in flight"}, test.WithTimestamp(now.Add(-2*time.Hour)))
		roomInfo, err := rsDB.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		eventTypeNID, err := rsDB.GetOrCreateEventTypeNID(ctx, inFlight.Type())
		require.NoError(t, err)
		eventStateKeyNID, err := rsDB.GetOrCreateEventStateKeyNID(ctx, inFlight.StateKey())
		require.NoError(t, err)
		_, _, err = rsDB.StoreEvent(ctx, inFlight.PDU, roomInfo, eventTypeNID, eventStateKeyNID, nil, false, false)
		require.NoError(t, err)

		var after rstypes.EventNID
		for {
			purged, next, done, err := rsDB.PurgeExpiredEvents(ctx, room.ID, now.Add(-time.Hour).UnixMilli(), after, 1)
			require.NoError(t, err)
			assert.NotContains(t, purged, inFlight.EventID())
			if done {
				break
			}
			after = next
		}
		nids, err := rsDB.EventNIDs(ctx, []string{inFlight.EventID()})
		require.NoError(t, err)
		assert.Contains(t, nids, inFlight.EventID(), "the purge deleted an event in flight")
	})
}
