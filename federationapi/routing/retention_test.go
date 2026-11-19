// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/federationapi/routing"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// MSC1763: /event, /backfill and /get_missing_events leave out events past
// the room's retention policy, as if the purge jobs had deleted them
// already, but keep state events and the room's forward extremities.
func TestRetentionFederation(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		ctx := context.Background()

		remote := spec.ServerName("remote.test")
		alice := test.NewUser(t)
		bob := test.NewUser(t, test.WithSigningServer(remote, "ed25519:remote", test.PrivateKeyB))
		room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV12), test.RoomPreset(test.PresetPublicChat))
		now := time.Now()
		message := func(body string, age time.Duration) string {
			return room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": body}, test.WithTimestamp(now.Add(-age))).EventID()
		}
		room.CreateAndInsert(t, alice, "m.room.retention", map[string]any{"max_lifetime": time.Hour.Milliseconds()}, test.WithStateKey(""))
		join := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID)).EventID()
		oldTopic := room.CreateAndInsert(t, alice, spec.MRoomTopic, map[string]any{"topic": "old"}, test.WithStateKey(""), test.WithTimestamp(now.Add(-2*time.Hour))).EventID()
		expired := message("two hours old", 2*time.Hour)
		kept := message("ten minutes old", 10*time.Minute)
		// The last event is the room's forward extremity, which is kept
		// even though it has expired.
		extremity := message("also two hours old", 2*time.Hour)
		require.NoError(t, rsapi.SendEvents(ctx, rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))

		getEvent := func(eventID string) int {
			req := fclient.NewFederationRequest(http.MethodGet, remote, cfg.Global.ServerName, "/_matrix/federation/v1/event/"+eventID)
			res := routing.GetEvent(ctx, &req, rsAPI, eventID, cfg.Global.ServerName, &cfg.Global.Retention)
			return res.Code
		}
		pduIDs := func(pdus gjson.Result) map[string]bool {
			ids := map[string]bool{}
			for _, pdu := range pdus.Array() {
				ev, err := gomatrixserverlib.MustGetRoomVersion(room.Version).NewEventFromTrustedJSON([]byte(pdu.Raw), false)
				require.NoError(t, err)
				ids[ev.EventID()] = true
			}
			return ids
		}
		backfill := func() map[string]bool {
			path := "/_matrix/federation/v1/backfill/" + room.ID + "?" + url.Values{"v": {extremity}, "limit": {"50"}}.Encode()
			req := fclient.NewFederationRequest(http.MethodGet, remote, cfg.Global.ServerName, path)
			httpReq, err := req.HTTPRequest()
			require.NoError(t, err)
			res := routing.Backfill(httpReq, &req, rsAPI, room.ID, &cfg.FederationAPI)
			require.Equal(t, http.StatusOK, res.Code)
			body, err := json.Marshal(res.JSON)
			require.NoError(t, err)
			return pduIDs(gjson.GetBytes(body, "pdus"))
		}
		missingEvents := func() map[string]bool {
			req := fclient.NewFederationRequest(http.MethodPost, remote, cfg.Global.ServerName, "/_matrix/federation/v1/get_missing_events/"+room.ID)
			require.NoError(t, req.SetContent(map[string]any{"earliest_events": []string{join}, "latest_events": []string{extremity}, "limit": 50}))
			httpReq, err := req.HTTPRequest()
			require.NoError(t, err)
			res := routing.GetMissingEvents(httpReq, &req, rsAPI, room.ID, &cfg.Global.Retention)
			require.Equal(t, http.StatusOK, res.Code)
			body, err := json.Marshal(res.JSON)
			require.NoError(t, err)
			return pduIDs(gjson.GetBytes(body, "events"))
		}

		for _, enabled := range []bool{false, true} {
			cfg.Global.Retention = config.Retention{Enabled: enabled}
			wantExpired := http.StatusOK
			if enabled {
				wantExpired = http.StatusNotFound
			}
			assert.Equal(t, wantExpired, getEvent(expired), "/event of the expired event, retention %v", enabled)
			for _, eventID := range []string{kept, extremity, oldTopic, join} {
				assert.Equal(t, http.StatusOK, getEvent(eventID), "/event of %s, retention %v", eventID, enabled)
			}

			backfilled := backfill()
			gotMissing := missingEvents()
			assert.Equal(t, !enabled, backfilled[expired], "/backfill served the expired event, retention %v", enabled)
			assert.Equal(t, !enabled, gotMissing[expired], "/get_missing_events served the expired event, retention %v", enabled)
			for _, eventID := range []string{kept, oldTopic} {
				assert.True(t, backfilled[eventID], "/backfill dropped %s, retention %v", eventID, enabled)
				assert.True(t, gotMissing[eventID], "/get_missing_events dropped %s, retention %v", eventID, enabled)
			}
			assert.True(t, backfilled[extremity], "/backfill dropped the forward extremity, retention %v", enabled)
		}
	})
}
