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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
	uapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// TestFullyReadMarkerOrdering covers MSC4446: m.fully_read only moves
// forward unless the client sends com.beeper.allow_backward.
func TestFullyReadMarkerOrdering(t *testing.T) {
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	first := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "1", "msgtype": "m.text"})
	second := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "2", "msgtype": "m.text"})
	third := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "3", "msgtype": "m.text"})
	otherRoom := test.NewRoom(t, alice)
	otherRoomEvent := otherRoom.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "x", "msgtype": "m.text"})

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
		require.NoError(t, api.SendEvents(context.Background(), rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
		require.NoError(t, api.SendEvents(context.Background(), rsAPI, api.KindNew, otherRoom.Events(), "test", "test", "test", nil, false))
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, nil, caching.DisableMetrics)

		accessTokens := map[*test.User]userDevice{alice: {}}
		createAccessTokens(t, accessTokens, userAPI, processCtx.Context(), routers)

		do := func(method, path string, body any) (int, gjson.Result) {
			t.Helper()
			opts := []test.HTTPRequestOpt{}
			if body != nil {
				opts = append(opts, test.WithJSONBody(t, body))
			}
			req := test.NewRequest(t, method, path, opts...)
			req.Header.Set("Authorization", "Bearer "+accessTokens[alice].accessToken)
			rec := httptest.NewRecorder()
			routers.Client.ServeHTTP(rec, req)
			return rec.Code, gjson.ParseBytes(rec.Body.Bytes())
		}
		roomPath := "/_matrix/client/v3/rooms/" + url.PathEscape(room.ID)
		readMarkers := func(body map[string]any) {
			t.Helper()
			code, res := do(http.MethodPost, roomPath+"/read_markers", body)
			require.Equal(t, http.StatusOK, code, res.Raw)
		}
		receipt := func(eventID string, body map[string]any) (int, gjson.Result) {
			t.Helper()
			path := roomPath + "/receipt/m.fully_read/" + url.PathEscape(eventID)
			if body == nil {
				return do(http.MethodPost, path, nil)
			}
			return do(http.MethodPost, path, body)
		}
		fullyRead := func() string {
			t.Helper()
			code, res := do(http.MethodGet, "/_matrix/client/v3/user/"+url.PathEscape(alice.ID)+"/rooms/"+url.PathEscape(room.ID)+"/account_data/m.fully_read", nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			return res.Get("event_id").Str
		}

		readMarkers(map[string]any{"m.fully_read": second.EventID()})
		require.Equal(t, second.EventID(), fullyRead())

		// Moving backwards without allow_backward is dropped with a 200.
		readMarkers(map[string]any{"m.fully_read": first.EventID()})
		require.Equal(t, second.EventID(), fullyRead(), "read_markers moved m.fully_read backwards")
		code, res := receipt(first.EventID(), map[string]any{})
		require.Equal(t, http.StatusOK, code, res.Raw)
		require.Equal(t, second.EventID(), fullyRead(), "receipt moved m.fully_read backwards")
		readMarkers(map[string]any{"m.fully_read": first.EventID(), "com.beeper.allow_backward": false})
		require.Equal(t, second.EventID(), fullyRead())

		// Moving forward works on both endpoints.
		code, res = receipt(third.EventID(), nil)
		require.Equal(t, http.StatusOK, code, res.Raw)
		require.Equal(t, third.EventID(), fullyRead())

		// allow_backward moves the marker backwards on both endpoints.
		readMarkers(map[string]any{"m.fully_read": second.EventID(), "com.beeper.allow_backward": true})
		require.Equal(t, second.EventID(), fullyRead())
		code, res = receipt(first.EventID(), map[string]any{"com.beeper.allow_backward": true})
		require.Equal(t, http.StatusOK, code, res.Raw)
		require.Equal(t, first.EventID(), fullyRead())

		// Re-sending the current marker is accepted.
		readMarkers(map[string]any{"m.fully_read": first.EventID()})
		require.Equal(t, first.EventID(), fullyRead())

		// Events that are unknown in the room are rejected like Synapse does,
		// with or without allow_backward.
		for _, eventID := range []string{"$unknown:test", otherRoomEvent.EventID()} {
			for _, body := range []map[string]any{{}, {"com.beeper.allow_backward": true}} {
				code, res = receipt(eventID, body)
				require.Equal(t, http.StatusNotFound, code, res.Raw)
				require.Equal(t, "M_UNKNOWN", res.Get("errcode").Str, res.Raw)
				body["m.fully_read"] = eventID
				code, res = do(http.MethodPost, roomPath+"/read_markers", body)
				require.Equal(t, http.StatusNotFound, code, res.Raw)
			}
		}
		require.Equal(t, first.EventID(), fullyRead())

		// A marker whose event is gone counts as older than any event.
		require.NoError(t, userAPI.InputAccountData(context.Background(), &uapi.InputAccountDataRequest{
			UserID: alice.ID, RoomID: room.ID, DataType: "m.fully_read", AccountData: []byte(`{"event_id":"$gone:test"}`),
		}, &uapi.InputAccountDataResponse{}))
		readMarkers(map[string]any{"m.fully_read": first.EventID()})
		require.Equal(t, first.EventID(), fullyRead())

		// Concurrent forward-only updates end on the newest event.
		events := []string{first.EventID(), second.EventID(), third.EventID()}
		var wg sync.WaitGroup
		for i := range 30 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := test.NewRequest(t, http.MethodPost, roomPath+"/read_markers", test.WithJSONBody(t, map[string]any{"m.fully_read": events[i%len(events)]}))
				req.Header.Set("Authorization", "Bearer "+accessTokens[alice].accessToken)
				rec := httptest.NewRecorder()
				routers.Client.ServeHTTP(rec, req)
				assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}()
		}
		wg.Wait()
		require.Equal(t, third.EventID(), fullyRead())
	})
}
