// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	fsAPI "codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// noJoinedHostsFederationAPI reports that no remote server is in any room.
type noJoinedHostsFederationAPI struct {
	fsAPI.RoomserverFederationAPI
}

func (noJoinedHostsFederationAPI) QueryJoinedHostServerNamesInRoom(
	_ context.Context, _ *fsAPI.QueryJoinedHostServerNamesInRoomRequest, _ *fsAPI.QueryJoinedHostServerNamesInRoomResponse,
) error {
	return nil
}

// A user sees the same rooms in a local space hierarchy as the room summary
// endpoint and remote spaces allow: any room that is world readable, that
// anyone may join or knock on, or that a room the user is in allows joining,
// and the children of every space the user may see. A remote server gets the
// children of a space it may see.
func TestRoomHierarchyAccessRules(t *testing.T) {
	ctx := context.Background()
	alice := test.NewUser(t)
	bob := test.NewUser(t)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(noJoinedHostsFederationAPI{}, nil)

		v10 := test.RoomVersion(gomatrixserverlib.RoomVersionV10)
		link := func(parent, child *test.Room) {
			parent.CreateAndInsert(t, alice, spec.MSpaceChild, map[string]any{
				"via": []string{string(cfg.Global.ServerName)},
			}, test.WithStateKey(child.ID))
		}
		joinRule := func(r *test.Room, rule string, allow ...string) {
			rules := []map[string]any{}
			for _, roomID := range allow {
				rules = append(rules, map[string]any{"type": spec.MRoomMembership, "room_id": roomID})
			}
			r.CreateAndInsert(t, alice, spec.MRoomJoinRules, map[string]any{"join_rule": rule, "allow": rules}, test.WithStateKey(""))
		}

		// bob is in lobby and space, and in nothing else.
		lobby := test.NewRoom(t, alice, v10)
		space := test.NewRoom(t, alice, v10, test.RoomType(spec.MSpace))
		subspace := test.NewRoom(t, alice, v10, test.RoomType(spec.MSpace))
		inSubspace := test.NewRoom(t, alice, v10)
		allowsLobby := test.NewRoom(t, alice, v10)
		joinRule(allowsLobby, spec.Restricted, lobby.ID)
		allowsOther := test.NewRoom(t, alice, v10)
		joinRule(allowsOther, spec.Restricted, "!elsewhere:"+string(cfg.Global.ServerName))
		inviteOnly := test.NewRoom(t, alice, v10)
		joinRule(inviteOnly, spec.Invite)
		link(space, subspace)
		link(space, allowsLobby)
		link(space, allowsOther)
		link(space, inviteOnly)
		link(subspace, inSubspace)
		// a public space bob has not joined
		notJoined := test.NewRoom(t, alice, v10, test.RoomType(spec.MSpace))
		notJoinedChild := test.NewRoom(t, alice, v10)
		link(notJoined, notJoinedChild)
		for _, r := range []*test.Room{lobby, space} {
			r.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		}
		for _, r := range []*test.Room{lobby, inSubspace, subspace, allowsLobby, allowsOther, inviteOnly, space, notJoinedChild, notJoined} {
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, r.Events(), "test", "test", "test", nil, false))
		}

		walk := func(caller types.DeviceOrServerName, root *test.Room, maxDepth int) (map[string][]string, []string) {
			t.Helper()
			rootID, err := spec.NewRoomID(root.ID)
			require.NoError(t, err)
			walker := api.NewRoomHierarchyWalker(caller, *rootID, false, maxDepth, nil)
			rooms, inaccessible, _, err := rsAPI.QueryNextRoomHierarchyPage(ctx, walker, -1)
			require.NoError(t, err)
			got := map[string][]string{}
			for _, r := range rooms {
				got[r.RoomID] = r.AllowedRoomIDs
			}
			return got, inaccessible
		}
		asBob := types.NewDeviceNotServerName(userapi.Device{UserID: bob.ID})

		rooms, inaccessible := walk(asBob, space, -1)
		require.Contains(t, rooms, subspace.ID, "a public subspace bob has not joined")
		require.Contains(t, rooms, inSubspace.ID, "a room in a subspace bob has not joined")
		require.Contains(t, rooms, allowsLobby.ID, "a restricted room that allows a room bob is in")
		require.Equal(t, []string{lobby.ID}, rooms[allowsLobby.ID])
		require.NotContains(t, rooms, allowsOther.ID)
		require.NotContains(t, rooms, inviteOnly.ID)
		require.ElementsMatch(t, []string{allowsOther.ID, inviteOnly.ID}, inaccessible)

		rooms, _ = walk(asBob, notJoined, -1)
		require.Contains(t, rooms, notJoined.ID, "a public space bob has not joined")
		require.Contains(t, rooms, notJoinedChild.ID)

		// Federation asks for one level and gets the space's children.
		rooms, inaccessible = walk(types.NewServerNameNotDevice("remote.example"), space, 1)
		require.Contains(t, rooms, space.ID)
		require.Contains(t, rooms, subspace.ID)
		require.NotContains(t, rooms, inSubspace.ID, "beyond the requested depth")
		require.NotContains(t, rooms, allowsLobby.ID, "the remote server is in no allowed room")
		require.Contains(t, inaccessible, inviteOnly.ID)
	})
}
