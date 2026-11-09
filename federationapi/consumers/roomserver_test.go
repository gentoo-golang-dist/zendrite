// Copyright 2024 New Vector Ltd.
// Copyright 2017 Vector Creations Ltd
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"context"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
)

func TestCombineNoOp(t *testing.T) {
	inputAdd1 := []string{"a", "b", "c"}
	inputDel1 := []string{"a", "b", "d"}
	inputAdd2 := []string{"a", "d", "e"}
	inputDel2 := []string{"a", "c", "e", "e"}

	gotAdd, gotDel := combineDeltas(inputAdd1, inputDel1, inputAdd2, inputDel2)

	if len(gotAdd) != 0 {
		t.Errorf("wanted combined adds to be an empty list, got %#v", gotAdd)
	}

	if len(gotDel) != 0 {
		t.Errorf("wanted combined removes to be an empty list, got %#v", gotDel)
	}
}

func TestCombineDedup(t *testing.T) {
	inputAdd1 := []string{"a", "a"}
	inputDel1 := []string{"b", "b"}
	inputAdd2 := []string{"a", "a"}
	inputDel2 := []string{"b", "b"}

	gotAdd, gotDel := combineDeltas(inputAdd1, inputDel1, inputAdd2, inputDel2)

	if len(gotAdd) != 1 || gotAdd[0] != "a" {
		t.Errorf("wanted combined adds to be %#v, got %#v", []string{"a"}, gotAdd)
	}

	if len(gotDel) != 1 || gotDel[0] != "b" {
		t.Errorf("wanted combined removes to be %#v, got %#v", []string{"b"}, gotDel)
	}
}

// senderRoomserverAPI resolves sender IDs as user IDs, as in rooms without
// pseudo IDs.
type senderRoomserverAPI struct {
	api.FederationRoomserverAPI
}

func (senderRoomserverAPI) QueryUserIDForSender(_ context.Context, _ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
	return spec.NewUserID(string(senderID), true)
}

// An event a remote server sent us through /send_leave or /send_join is not
// sent back to that server, while our own events go to every joined server.
func TestRemoteSenderServerIsNotSentItsOwnEvent(t *testing.T) {
	alice := test.NewUser(t)
	charlie := test.NewUser(t, test.WithSigningServer("remote", "ed25519:remote", test.PrivateKeyB))
	room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
	room.CreateAndInsert(t, charlie, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(charlie.ID))
	leave := room.CreateAndInsert(t, charlie, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(charlie.ID))
	message := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "hi"})

	s := &OutputRoomEventConsumer{
		ctx:   context.Background(),
		cfg:   &config.FederationAPI{Matrix: &config.Global{ServerName: "test"}},
		rsAPI: senderRoomserverAPI{},
	}

	server, ok := s.remoteSenderServer(api.OutputNewRoomEvent{Event: leave, SendAsServer: "test"})
	require.True(t, ok)
	require.Equal(t, spec.ServerName("remote"), server)
	require.Equal(t, []spec.ServerName{"other"},
		withoutServers([]spec.ServerName{"remote", "other"}, []spec.ServerName{server}))

	_, ok = s.remoteSenderServer(api.OutputNewRoomEvent{Event: message, SendAsServer: "test"})
	require.False(t, ok, "a local event has no remote sender to leave out")
}
