// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sync

import (
	"encoding/json"
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

// TestSlidingSyncInviteBlocking checks that sliding sync serves no invites to
// a user who blocks them (MSC4380), in room lists and room subscriptions, and
// serves them again once the user stops blocking.
func TestSlidingSyncInviteBlocking(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		db, err := storage.NewSyncServerDatasource(processCtx.Context(), cm, &cfg.SyncAPI.Database)
		require.NoError(t, err)
		ctx := t.Context()

		room := test.NewRoom(t, alice)
		invite := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(bob.ID))
		// The roomserver names the invitee when it outputs the invite.
		invitee, err := spec.NewUserID(bob.ID, true)
		require.NoError(t, err)
		invite.UserID = *invitee
		invitePos, err := db.AddInviteEvent(ctx, invite)
		require.NoError(t, err)

		userAPI := &mockUserAPI{globalAccountData: map[string]json.RawMessage{}}
		rp := &RequestPool{db: db, userAPI: userAPI}
		served := func() (listed bool, membership string) {
			t.Helper()
			rooms, roomsErr := rp.GetRoomsForUser(ctx, bob.ID, spec.Invite)
			require.NoError(t, roomsErr)
			snapshot, snapshotErr := db.NewDatabaseSnapshot(ctx)
			require.NoError(t, snapshotErr)
			defer func() { _ = snapshot.Rollback() }()
			result := rp.detectRoomMembership(ctx, snapshot, room.ID, bob.ID, types.StreamingToken{InvitePosition: invitePos})
			return len(rooms) == 1 && rooms[0].RoomID == room.ID, result.membership
		}

		listed, membership := served()
		require.True(t, listed)
		require.Equal(t, spec.Invite, membership)

		for _, dataType := range []string{userapi.InvitePermissionConfigType, userapi.UnstableInvitePermissionConfigType} {
			userAPI.globalAccountData = map[string]json.RawMessage{dataType: json.RawMessage(`{"default_action":"block"}`)}
			listed, membership = served()
			require.False(t, listed, dataType)
			require.NotEqual(t, spec.Invite, membership, dataType)
		}

		userAPI.globalAccountData = map[string]json.RawMessage{userapi.InvitePermissionConfigType: json.RawMessage(`{}`)}
		listed, membership = served()
		require.True(t, listed)
		require.Equal(t, spec.Invite, membership)
	})
}
