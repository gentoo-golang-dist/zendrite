// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package syncapi

import (
	"encoding/json"
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

// TestSyncInviteFiltering checks that /sync leaves out invites the invitee
// ignores or blocks (MSC4155) and serves the others.
func TestSyncInviteFiltering(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		charlie := test.NewUser(t)
		bob := test.NewUser(t)
		invitee, err := spec.NewUserID(bob.ID, true)
		require.NoError(t, err)
		aliceRoom := test.NewRoom(t, alice)
		charlieRoom := test.NewRoom(t, charlie)
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
		AddPublicRoutes(processCtx, routers, cfg, cm, &natsInstance, userAPI, &syncRoomserverAPI{rooms: []*test.Room{aliceRoom, charlieRoom}}, caches, caching.DisableMetrics, nil, nil)
		for _, inv := range []struct {
			room   *test.Room
			sender *test.User
		}{{aliceRoom, alice}, {charlieRoom, charlie}} {
			invite := inv.room.CreateAndInsert(t, inv.sender, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(bob.ID))
			invite.UserID = *invitee
			testrig.MustPublishMsgs(t, jsctx, testrig.NewOutputEventMsg(t, cfg, inv.room.ID, rsapi.OutputEvent{
				Type:           rsapi.OutputTypeNewInviteEvent,
				NewInviteEvent: &rsapi.OutputNewInviteEvent{Event: invite, RoomVersion: inv.room.Version},
			}))
		}

		alicePath := "rooms.invite." + gjson.Escape(aliceRoom.ID)
		charliePath := "rooms.invite." + gjson.Escape(charlieRoom.ID)
		syncUntil(t, routers, bobDevice.AccessToken, false, func(syncBody string) bool {
			return gjson.Get(syncBody, alicePath).Exists() && gjson.Get(syncBody, charliePath).Exists()
		})

		for _, tc := range []struct {
			name                   string
			config                 map[string]string
			wantAlice, wantCharlie bool
		}{
			{name: "ignored user", config: map[string]string{
				userapi.MSC4155InvitePermissionConfigType: `{"ignored_users":["` + alice.ID + `"]}`,
			}, wantCharlie: true},
			{name: "blocked server with allowed user", config: map[string]string{
				userapi.MSC4155InvitePermissionConfigType: `{"allowed_users":["` + charlie.ID + `"],"blocked_servers":["*"]}`,
			}, wantCharlie: true},
			{name: "ignored server", config: map[string]string{
				userapi.MSC4155InvitePermissionConfigType: `{"ignored_servers":["test"]}`,
			}},
			{name: "disabled", config: map[string]string{
				userapi.MSC4155InvitePermissionConfigType: `{"enabled":false,"ignored_servers":["*"]}`,
			}, wantAlice: true, wantCharlie: true},
			// A stated MSC4380 preference takes precedence.
			{name: "MSC4380 allow", config: map[string]string{
				userapi.InvitePermissionConfigType:        `{"default_action":"allow"}`,
				userapi.MSC4155InvitePermissionConfigType: `{"ignored_servers":["*"]}`,
			}, wantAlice: true, wantCharlie: true},
			{name: "no configuration", wantAlice: true, wantCharlie: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				config := map[string]json.RawMessage{}
				for dataType, content := range tc.config {
					config[dataType] = json.RawMessage(content)
				}
				userAPI.globalAccountData[bob.ID] = config
				body := fullSync(t, routers, bobDevice.AccessToken)
				require.Equal(t, tc.wantAlice, gjson.Get(body, alicePath).Exists(), body)
				require.Equal(t, tc.wantCharlie, gjson.Get(body, charliePath).Exists(), body)
			})
		}
	})
}
