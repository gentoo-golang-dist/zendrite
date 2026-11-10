// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package query

import (
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"

	"codefloe.com/pat-s/zendrite/test"
)

func pduKeys(events []gomatrixserverlib.PDU) []string {
	keys := make([]string, 0, len(events))
	for _, ev := range events {
		keys = append(keys, ev.Type()+"|"+*ev.StateKey())
	}
	return keys
}

func roomState(room *test.Room) []gomatrixserverlib.PDU {
	var state []gomatrixserverlib.PDU
	for _, ev := range room.CurrentState() {
		state = append(state, ev.PDU)
	}
	return state
}

func TestPartialJoinStateHeroes(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	carol := test.NewUser(t)
	joiner := test.NewUser(t)

	t.Run("left members are heroes when nobody else is in the room", func(t *testing.T) {
		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(bob.ID))
		room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Ban}, test.WithStateKey(carol.ID))
		room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(alice.ID))

		assert.ElementsMatch(t, []string{
			"m.room.create|", "m.room.power_levels|", "m.room.join_rules|", "m.room.history_visibility|",
			"m.room.member|" + alice.ID, "m.room.member|" + bob.ID, "m.room.member|" + carol.ID,
		}, pduKeys(partialJoinState(roomState(room), joiner.ID, nil)))
	})

	t.Run("the joining user is never a hero", func(t *testing.T) {
		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(joiner.ID))
		var users []*test.User
		for range 6 {
			u := test.NewUser(t)
			users = append(users, u)
			room.CreateAndInsert(t, u, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(u.ID))
		}

		// The joiner's invite is kept as their own membership, and the five
		// heroes are alice and the four oldest other members.
		assert.ElementsMatch(t, []string{
			"m.room.create|", "m.room.power_levels|", "m.room.join_rules|", "m.room.history_visibility|",
			"m.room.member|" + joiner.ID, "m.room.member|" + alice.ID,
			"m.room.member|" + users[0].ID, "m.room.member|" + users[1].ID,
			"m.room.member|" + users[2].ID, "m.room.member|" + users[3].ID,
		}, pduKeys(partialJoinState(roomState(room), joiner.ID, nil)))
	})

	t.Run("a canonical alias names the room", func(t *testing.T) {
		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, alice, spec.MRoomCanonicalAlias, map[string]any{"alias": "#a:test"}, test.WithStateKey(""))
		// A name with a non-empty state key does not name the room.
		room.CreateAndInsert(t, alice, spec.MRoomName, map[string]any{"name": "n"}, test.WithStateKey("x"))

		assert.ElementsMatch(t, []string{
			"m.room.create|", "m.room.power_levels|", "m.room.join_rules|", "m.room.history_visibility|",
			"m.room.canonical_alias|", "m.room.name|x",
		}, pduKeys(partialJoinState(roomState(room), joiner.ID, nil)))
	})
}

func TestPartialJoinAuthChain(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
	bobJoin := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))

	var known, state []gomatrixserverlib.PDU
	for _, ev := range room.Events() {
		known = append(known, ev.PDU)
		if ev.Type() == spec.MRoomCreate || ev.EventID() == bobJoin.EventID() {
			state = append(state, ev.PDU)
		}
	}

	// Bob's join is authorized by the join rules, power levels and, through
	// them, alice's join; the create event is in the state.
	chain := partialJoinAuthChain(state, nil, known)
	assert.ElementsMatch(t, []string{
		"m.room.member|" + alice.ID, "m.room.power_levels|", "m.room.join_rules|",
	}, pduKeys(chain))

	// The join's auth events are walked too, and unknown ones are skipped.
	chain = partialJoinAuthChain(state[:1], []string{bobJoin.EventID(), "$unknown"}, known)
	assert.ElementsMatch(t, []string{
		"m.room.member|" + bob.ID, "m.room.member|" + alice.ID, "m.room.power_levels|", "m.room.join_rules|",
	}, pduKeys(chain))
}
