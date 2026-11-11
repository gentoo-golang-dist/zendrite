// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"context"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"

	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/test"
)

type leaveMembersRoomserverAPI struct {
	rsapi.UserRoomserverAPI
	members []string
}

func (a *leaveMembersRoomserverAPI) QueryMembershipsForRoom(
	_ context.Context, _ *rsapi.QueryMembershipsForRoomRequest, res *rsapi.QueryMembershipsForRoomResponse,
) error {
	for _, member := range a.members {
		res.JoinEvents = append(res.JoinEvents, synctypes.ClientEvent{StateKey: &member})
	}
	return nil
}

// TestForgetLeftDeviceLists checks which users' device lists a leave hands
// on to be dropped, should they share no other room: the remote user who
// left, or the remote members of a room a local user left.
func TestForgetLeftDeviceLists(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t, test.WithSigningServer("remote", "ed25519:remote", test.PrivateKeyB))
	room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
	room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
	bobLeave := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(bob.ID))
	message := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "hi"})
	aliceLeave := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(alice.ID))

	var forgotten [][]string
	s := &OutputRoomEventConsumer{
		cfg:   &config.UserAPI{Matrix: &config.Global{SigningIdentity: fclient.SigningIdentity{ServerName: "test"}}},
		rsAPI: &leaveMembersRoomserverAPI{members: []string{bob.ID, "@carol:test"}},
		forgetUntracked: func(_ context.Context, userIDs []string) error {
			forgotten = append(forgotten, userIDs)
			return nil
		},
	}
	ctx := context.Background()
	for _, output := range []*rsapi.OutputNewRoomEvent{
		{Event: bobLeave, AddsStateEventIDs: []string{bobLeave.EventID()}},
		{Event: message},
		{Event: aliceLeave, AddsStateEventIDs: []string{aliceLeave.EventID()}},
		// A leave that does not change the state, such as one that lost
		// state resolution, changes nothing.
		{Event: aliceLeave},
	} {
		s.forgetLeftDeviceLists(ctx, output)
	}
	assert.Equal(t, [][]string{{bob.ID}, {bob.ID}}, forgotten)
}
