// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package syncapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// TestSyncInviteBlocking checks that /sync serves no invites to a user who
// blocks them (MSC4380), and serves them again once the user stops blocking.
func TestSyncInviteBlocking(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		room := test.NewRoom(t, alice)
		invite := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(bob.ID))
		invitee, err := spec.NewUserID(bob.ID, true)
		require.NoError(t, err)
		invite.UserID = *invitee
		bobDevice := userapi.Device{ID: "BOBID", UserID: bob.ID, AccessToken: "BOB_BEARER_TOKEN", AccountType: userapi.AccountTypeUser}

		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)

		userAPI := &syncUserAPI{
			accounts:          []userapi.Device{bobDevice},
			globalAccountData: map[string]map[string]json.RawMessage{},
		}
		AddPublicRoutes(processCtx, routers, cfg, cm, &natsInstance, userAPI, &syncRoomserverAPI{rooms: []*test.Room{room}}, caches, caching.DisableMetrics, nil, nil)
		testrig.MustPublishMsgs(t, jsctx, testrig.NewOutputEventMsg(t, cfg, room.ID, rsapi.OutputEvent{
			Type:           rsapi.OutputTypeNewInviteEvent,
			NewInviteEvent: &rsapi.OutputNewInviteEvent{Event: invite, RoomVersion: room.Version},
		}))

		invitePath := "rooms.invite." + gjson.Escape(room.ID)
		syncUntil(t, routers, bobDevice.AccessToken, false, func(syncBody string) bool {
			return gjson.Get(syncBody, invitePath).Exists()
		})

		for _, config := range []map[string]json.RawMessage{
			{userapi.InvitePermissionConfigType: json.RawMessage(`{"default_action":"block"}`)},
			{userapi.UnstableInvitePermissionConfigType: json.RawMessage(`{"default_action":"block"}`)},
		} {
			userAPI.globalAccountData[bob.ID] = config
			require.False(t, gjson.Get(fullSync(t, routers, bobDevice.AccessToken), invitePath).Exists(), config)
		}

		// The stable type takes precedence over the unstable one.
		userAPI.globalAccountData[bob.ID] = map[string]json.RawMessage{
			userapi.InvitePermissionConfigType:         json.RawMessage(`{"default_action":"allow"}`),
			userapi.UnstableInvitePermissionConfigType: json.RawMessage(`{"default_action":"block"}`),
		}
		require.True(t, gjson.Get(fullSync(t, routers, bobDevice.AccessToken), invitePath).Exists())
	})
}

// fullSync makes an initial /sync request and returns the response body.
func fullSync(t *testing.T, routers httputil.Routers, accessToken string) string {
	t.Helper()
	w := httptest.NewRecorder()
	routers.Client.ServeHTTP(w, test.NewRequest(t, "GET", "/_matrix/client/v3/sync", test.WithQueryParams(map[string]string{
		"access_token": accessToken,
		"timeout":      "0",
	})))
	require.Equal(t, 200, w.Code, w.Body.String())
	return w.Body.String()
}
