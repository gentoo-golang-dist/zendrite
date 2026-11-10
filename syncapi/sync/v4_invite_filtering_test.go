// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sync

import (
	"encoding/json"
	"sort"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// TestSlidingSyncInviteFiltering checks that sliding sync leaves out invites
// the invitee ignores or blocks (MSC4155) or that come from a user in
// m.ignored_user_list, in room lists and room subscriptions.
func TestSlidingSyncInviteFiltering(t *testing.T) {
	alice := test.NewUser(t)
	charlie := test.NewUser(t)
	bob := test.NewUser(t)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		db, err := storage.NewSyncServerDatasource(processCtx.Context(), cm, &cfg.SyncAPI.Database)
		require.NoError(t, err)
		ctx := t.Context()

		invitee, err := spec.NewUserID(bob.ID, true)
		require.NoError(t, err)
		aliceRoom := test.NewRoom(t, alice)
		charlieRoom := test.NewRoom(t, charlie)
		var invitePos types.StreamPosition
		for _, inv := range []struct {
			room   *test.Room
			sender *test.User
		}{{aliceRoom, alice}, {charlieRoom, charlie}} {
			invite := inv.room.CreateAndInsert(t, inv.sender, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(bob.ID))
			invite.UserID = *invitee
			invitePos, err = db.AddInviteEvent(ctx, invite)
			require.NoError(t, err)
		}

		userAPI := &mockUserAPI{globalAccountData: map[string]json.RawMessage{}}
		rp := &RequestPool{db: db, userAPI: userAPI, rsAPI: &mockRoomserverAPI{}}
		// served returns the invited rooms in the room list and those that
		// a room subscription sees as invites.
		served := func() (listed, subscribed []string) {
			t.Helper()
			rooms, roomsErr := rp.GetRoomsForUser(ctx, bob.ID, spec.Invite)
			require.NoError(t, roomsErr)
			for _, room := range rooms {
				listed = append(listed, room.RoomID)
			}
			sort.Strings(listed)
			snapshot, snapshotErr := db.NewDatabaseSnapshot(ctx)
			require.NoError(t, snapshotErr)
			defer func() { _ = snapshot.Rollback() }()
			for _, roomID := range []string{aliceRoom.ID, charlieRoom.ID} {
				result := rp.detectRoomMembership(ctx, snapshot, roomID, bob.ID, types.StreamingToken{InvitePosition: invitePos})
				if result.membership == spec.Invite {
					subscribed = append(subscribed, roomID)
				}
			}
			sort.Strings(subscribed)
			return listed, subscribed
		}
		both := []string{aliceRoom.ID, charlieRoom.ID}
		sort.Strings(both)
		requireServed := func(want []string) {
			t.Helper()
			listed, subscribed := served()
			require.Equal(t, want, listed, "room list")
			require.Equal(t, want, subscribed, "room subscription")
		}

		requireServed(both)

		for _, tc := range []struct {
			name    string
			content string
			want    []string
		}{
			{"ignored user", `{"ignored_users":["` + alice.ID + `"]}`, []string{charlieRoom.ID}},
			{"blocked user", `{"blocked_users":["` + charlie.ID + `"]}`, []string{aliceRoom.ID}},
			{"blocked server with allowed user", `{"allowed_users":["` + charlie.ID + `"],"blocked_servers":["*"]}`, []string{charlieRoom.ID}},
			{"ignored server", `{"ignored_servers":["test"]}`, nil},
			{"disabled", `{"enabled":false,"ignored_servers":["*"]}`, both},
		} {
			t.Run(tc.name, func(t *testing.T) {
				userAPI.globalAccountData = map[string]json.RawMessage{userapi.MSC4155InvitePermissionConfigType: json.RawMessage(tc.content)}
				requireServed(tc.want)
			})
		}

		// A stated MSC4380 preference takes precedence.
		userAPI.globalAccountData = map[string]json.RawMessage{
			userapi.InvitePermissionConfigType:        json.RawMessage(`{"default_action":"allow"}`),
			userapi.MSC4155InvitePermissionConfigType: json.RawMessage(`{"ignored_servers":["*"]}`),
		}
		requireServed(both)

		// Invites from users in m.ignored_user_list are left out too.
		userAPI.globalAccountData = map[string]json.RawMessage{}
		require.NoError(t, db.UpdateIgnoresForUser(ctx, bob.ID, &types.IgnoredUsers{List: map[string]any{alice.ID: struct{}{}}}))
		requireServed([]string{charlieRoom.ID})
	})
}
