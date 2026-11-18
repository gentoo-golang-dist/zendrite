// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// MSC4370: a server in the room gets the current forward extremities, and
// servers outside it or denied by the ACL do not.
func TestGetExtremities(t *testing.T) {
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
		send := func(events ...*types.HeaderedEvent) {
			require.NoError(t, rsapi.SendEvents(ctx, rsAPI, rsapi.KindNew, events, "test", "test", "test", nil, false))
		}
		send(room.Events()...)

		get := func(roomID string) (int, []byte) {
			req := fclient.NewFederationRequest(http.MethodGet, remote, cfg.Global.ServerName, "/_matrix/federation/unstable/org.matrix.msc4370/extremities/"+roomID)
			httpReq, err := req.HTTPRequest()
			require.NoError(t, err)
			res := routing.GetExtremities(httpReq, &req, rsAPI, roomID)
			body, err := json.Marshal(res.JSON)
			require.NoError(t, err)
			return res.Code, body
		}
		wantError := func(roomID string, code int, errcode spec.MatrixErrorCode, msg string) {
			t.Helper()
			gotCode, body := get(roomID)
			assert.Equal(t, code, gotCode, string(body))
			assert.Equal(t, string(errcode), gjson.GetBytes(body, "errcode").Str, string(body))
			if msg != "" {
				assert.Equal(t, msg, gjson.GetBytes(body, "error").Str, string(body))
			}
		}

		// No user of the requesting server has joined yet.
		wantError(room.ID, http.StatusForbidden, spec.ErrorForbidden, "Host not in room.")
		wantError("!"+strings.Repeat("B", 43), http.StatusNotFound, spec.ErrorNotFound, "")
		wantError("not-a-room-id", http.StatusBadRequest, spec.ErrorInvalidParam, "")

		join := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		send(join)
		code, body := get(room.ID)
		require.Equal(t, http.StatusOK, code, string(body))
		assert.JSONEq(t, `{"prev_events":["`+join.EventID()+`"]}`, string(body))

		// A second extremity is reported as it is, not resolved into one.
		other := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "fork"}, test.WithPrevEvents(join.PrevEventIDs()...))
		send(other)
		code, body = get(room.ID)
		require.Equal(t, http.StatusOK, code, string(body))
		var res struct {
			PrevEvents []string `json:"prev_events"`
		}
		require.NoError(t, json.Unmarshal(body, &res))
		assert.ElementsMatch(t, []string{join.EventID(), other.EventID()}, res.PrevEvents)

		acl := room.CreateAndInsert(t, alice, "m.room.server_acl", map[string]any{
			"allow": []string{"*"}, "deny": []string{string(remote)}, "allow_ip_literals": false,
		}, test.WithStateKey(""))
		send(acl)
		wantError(room.ID, http.StatusForbidden, spec.ErrorForbidden, "Server is banned from room.")
	})
}

// MSC4370: the endpoint is only served with the MSC enabled.
func TestGetExtremitiesNeedsMSC(t *testing.T) {
	cfg, _, closeDB := testrig.CreateConfig(t, test.DBTypeSQLite)
	defer closeDB()
	for _, enabled := range []bool{false, true} {
		cfg.MSCs.MSCs = nil
		if enabled {
			cfg.MSCs.MSCs = []string{"msc4370"}
		}
		routers := httputil.NewRouters()
		routing.Setup(routers, cfg, nil, nil, nil, nil, nil, &cfg.MSCs, nil, false)
		rec := httptest.NewRecorder()
		routers.Federation.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_matrix/federation/unstable/org.matrix.msc4370/extremities/!room:test", nil))
		if enabled {
			// The unsigned request reaches the handler and fails authentication.
			assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		} else {
			assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			assert.Equal(t, "M_UNRECOGNIZED", gjson.Get(rec.Body.String(), "errcode").Str)
		}
	}
}
