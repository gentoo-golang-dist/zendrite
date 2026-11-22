// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package perform_test

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fsAPI "codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	rsAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// joinFederationAPI records the federated joins asked of the federation API
// and reports each as made through the first server named.
type joinFederationAPI struct {
	fsAPI.RoomserverFederationAPI
	joinReqs []fsAPI.PerformJoinRequest
}

func (f *joinFederationAPI) PerformJoin(_ context.Context, req *fsAPI.PerformJoinRequest, res *fsAPI.PerformJoinResponse) {
	f.joinReqs = append(f.joinReqs, *req)
	res.JoinedVia = req.ServerNames[0]
}

// TestGuestJoinWithFederatedInvite checks that a guest invited to a room on
// another server is let in, or refused, by the m.room.guest_access event in
// the invite's stripped state, as this server has no state of its own for
// the room (Sytest "Guest users can accept invites to private rooms over
// federation").
func TestGuestJoinWithFederatedInvite(t *testing.T) {
	remote := spec.ServerName("remote.test")
	for _, tc := range []struct {
		name        string
		guestAccess string
		wantJoin    bool
	}{
		{name: "can_join", guestAccess: "can_join", wantJoin: true},
		{name: "forbidden", guestAccess: "forbidden", wantJoin: false},
		{name: "absent", guestAccess: "", wantJoin: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
				creator := test.NewUser(t, test.WithSigningServer(remote, "ed25519:remote", test.PrivateKeyB))
				guest := test.NewUser(t)
				room := test.NewRoom(t, creator, test.RoomPreset(test.PresetPrivateChat))
				if tc.guestAccess != "" {
					room.CreateAndInsert(t, creator, spec.MRoomGuestAccess, map[string]any{"guest_access": tc.guestAccess}, test.WithStateKey(""))
				}
				var state []gomatrixserverlib.InviteStrippedState
				for _, ev := range room.CurrentState() {
					if ev.Type() == spec.MRoomMember {
						continue
					}
					state = append(state, gomatrixserverlib.NewInviteStrippedState(ev.PDU))
				}
				invite := room.CreateEvent(t, creator, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(guest.ID))
				require.NoError(t, invite.SetUnsignedField("invite_room_state", state))
				ctx := context.Background()

				cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
				defer closeDB()
				cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
				caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
				natsInstance := jetstream.NATSInstance{}
				jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
				defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)
				internalAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
				fedAPI := &joinFederationAPI{}
				internalAPI.SetFederationAPI(fedAPI, nil)
				require.NoError(t, internalAPI.HandleInvite(ctx, invite))

				roomID, joinedVia, err := internalAPI.PerformJoin(ctx, &rsAPI.PerformJoinRequest{
					RoomIDOrAlias: room.ID,
					UserID:        guest.ID,
					IsGuest:       true,
				})
				if !tc.wantJoin {
					var notAllowed rsAPI.ErrNotAllowed
					require.ErrorAs(t, err, &notAllowed)
					assert.Empty(t, fedAPI.joinReqs, "a refused guest is not joined over federation")
					return
				}
				require.NoError(t, err)
				assert.Equal(t, room.ID, roomID)
				assert.Equal(t, remote, joinedVia)
				require.Len(t, fedAPI.joinReqs, 1)
				assert.Contains(t, fedAPI.joinReqs[0].ServerNames, remote)
			})
		})
	}
}
