// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/fclient"
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

// hierarchyFederationAPI answers /hierarchy with one room per server.
type hierarchyFederationAPI struct {
	fsAPI.RoomserverFederationAPI
	rooms map[spec.ServerName]fclient.RoomHierarchyRoom
	asked []spec.ServerName
}

func (f *hierarchyFederationAPI) RoomHierarchies(
	_ context.Context, _, dst spec.ServerName, _ string, _ bool,
) (fclient.RoomHierarchyResponse, error) {
	f.asked = append(f.asked, dst)
	room, ok := f.rooms[dst]
	if !ok {
		return fclient.RoomHierarchyResponse{}, &fsAPI.FederationClientError{Code: 404}
	}
	return fclient.RoomHierarchyResponse{Room: room}, nil
}

// A client can walk a space that this server is not in by naming servers
// to ask for it (MSC4235), as long as the user may see the space.
func TestRoomHierarchyVia(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)

		space := func(roomID, joinRule string) fclient.RoomHierarchyRoom {
			spaceType := spec.MSpace
			return fclient.RoomHierarchyRoom{
				PublicRoom:    fclient.PublicRoom{RoomID: roomID, JoinRule: joinRule},
				RoomType:      &spaceType,
				ChildrenState: []fclient.RoomHierarchyStrippedEvent{},
			}
		}
		federation := &hierarchyFederationAPI{rooms: map[spec.ServerName]fclient.RoomHierarchyRoom{
			"public.example":  space("!public:public.example", spec.Public),
			"private.example": space("!private:private.example", spec.Invite),
		}}
		rsAPI.SetFederationAPI(federation, nil)
		caller := types.NewDeviceNotServerName(userapi.Device{UserID: "@alice:" + string(cfg.Global.ServerName)})

		walk := func(roomID string, vias ...string) ([]fclient.RoomHierarchyRoom, error) {
			parsed, err := spec.NewRoomID(roomID)
			require.NoError(t, err)
			walker := api.NewRoomHierarchyWalker(caller, *parsed, false, -1, vias)
			rooms, _, _, err := rsAPI.QueryNextRoomHierarchyPage(ctx, walker, 50)
			return rooms, err
		}

		_, err := walk("!public:public.example")
		require.ErrorAs(t, err, &api.ErrRoomUnknownOrNotAllowed{}, "an unknown room without vias")
		require.Empty(t, federation.asked)

		rooms, err := walk("!public:public.example", "down.example", "public.example")
		require.NoError(t, err)
		require.Len(t, rooms, 1)
		require.Equal(t, "!public:public.example", rooms[0].RoomID)

		_, err = walk("!private:private.example", "private.example")
		require.ErrorAs(t, err, &api.ErrRoomUnknownOrNotAllowed{}, "an invite-only room the user is not invited to")
	})
}
