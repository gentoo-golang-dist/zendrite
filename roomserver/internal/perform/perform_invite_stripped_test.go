// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package perform

import (
	"context"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/test"
)

// stateByTuple is a state querier over a fixed set of state events.
type stateByTuple struct {
	gomatrixserverlib.StateQuerier
	events map[gomatrixserverlib.StateKeyTuple]gomatrixserverlib.PDU
}

func (s stateByTuple) GetState(_ context.Context, _ spec.RoomID, wanted []gomatrixserverlib.StateKeyTuple) ([]gomatrixserverlib.PDU, error) {
	var events []gomatrixserverlib.PDU
	for _, tuple := range wanted {
		if ev, ok := s.events[tuple]; ok {
			events = append(events, ev)
		}
	}
	return events, nil
}

// TestInviteStrippedState checks that an invite's stripped state includes
// the topic, the guest access rule and the inviter's membership (MSC4319),
// and no other member.
func TestInviteStrippedState(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	room := test.NewRoom(t, alice)
	room.CreateAndInsert(t, alice, spec.MRoomTopic, map[string]any{"topic": "Lunch"}, test.WithStateKey(""))
	room.CreateAndInsert(t, alice, spec.MRoomGuestAccess, map[string]any{"guest_access": "can_join"}, test.WithStateKey(""))
	room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))

	querier := stateByTuple{events: map[gomatrixserverlib.StateKeyTuple]gomatrixserverlib.PDU{}}
	for _, ev := range room.CurrentState() {
		querier.events[gomatrixserverlib.StateKeyTuple{EventType: ev.Type(), StateKey: *ev.StateKey()}] = ev.PDU
	}
	roomID, err := spec.NewRoomID(room.ID)
	require.NoError(t, err)

	stripped, err := inviteStrippedState(t.Context(), querier, *roomID, spec.SenderID(alice.ID))
	require.NoError(t, err)

	members := map[string]bool{}
	types := map[string]bool{}
	for _, ev := range stripped {
		types[ev.Type()] = true
		if ev.Type() == spec.MRoomMember {
			members[*ev.StateKey()] = true
		}
	}
	assert.True(t, types[spec.MRoomCreate])
	assert.True(t, types[spec.MRoomTopic], "the topic is part of the stripped state")
	assert.True(t, types[spec.MRoomGuestAccess], "the guest access rule is part of the stripped state")
	assert.Equal(t, map[string]bool{alice.ID: true}, members, "the inviter's membership, and only it")
}
