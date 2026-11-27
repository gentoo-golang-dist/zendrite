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

// A space member who has not joined a child room sees it in the space's
// hierarchy when its join rule is knock_restricted, as with knock and with
// restricted rooms that allow the space.
func TestRoomHierarchyKnockRestrictedChild(t *testing.T) {
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
		rsAPI.SetFederationAPI(nil, nil)

		space := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10), test.RoomType(spec.MSpace))
		child := func(joinRule string, allow ...string) *test.Room {
			r := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
			content := map[string]any{"join_rule": joinRule}
			if len(allow) > 0 {
				rules := []map[string]any{}
				for _, roomID := range allow {
					rules = append(rules, map[string]any{"type": spec.MRoomMembership, "room_id": roomID})
				}
				content["allow"] = rules
			}
			r.CreateAndInsert(t, alice, spec.MRoomJoinRules, content, test.WithStateKey(""))
			space.CreateAndInsert(t, alice, spec.MSpaceChild, map[string]any{
				"via": []string{string(cfg.Global.ServerName)},
			}, test.WithStateKey(r.ID))
			return r
		}
		knockRestricted := child(spec.KnockRestricted, space.ID)
		knockRestrictedOtherAllow := child(spec.KnockRestricted, "!elsewhere:"+string(cfg.Global.ServerName))
		restricted := child(spec.Restricted, space.ID)
		invite := child(spec.Invite)
		space.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))

		for _, r := range []*test.Room{knockRestricted, knockRestrictedOtherAllow, restricted, invite, space} {
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, r.Events(), "test", "test", "test", nil, false))
		}

		spaceID, err := spec.NewRoomID(space.ID)
		require.NoError(t, err)
		caller := types.NewDeviceNotServerName(userapi.Device{UserID: bob.ID})
		walker := api.NewRoomHierarchyWalker(caller, *spaceID, false, -1, nil)
		rooms, inaccessible, _, err := rsAPI.QueryNextRoomHierarchyPage(ctx, walker, 50)
		require.NoError(t, err)

		allowed := map[string][]string{}
		for _, r := range rooms {
			allowed[r.RoomID] = r.AllowedRoomIDs
		}
		require.Contains(t, allowed, space.ID)
		require.Contains(t, allowed, knockRestricted.ID)
		require.Equal(t, []string{space.ID}, allowed[knockRestricted.ID])
		require.Contains(t, allowed, knockRestrictedOtherAllow.ID, "anyone may knock, whatever the allow list")
		require.Contains(t, allowed, restricted.ID)
		require.Equal(t, []string{space.ID}, allowed[restricted.ID])
		require.NotContains(t, allowed, invite.ID)
		require.Contains(t, inaccessible, invite.ID)
	})
}
