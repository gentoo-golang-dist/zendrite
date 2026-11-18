// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"net/http"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
)

// MSC3848: the errcode names the check that refuses the event, following the
// checks Synapse tags.
func TestMSC3848Errcode(t *testing.T) {
	for _, version := range []gomatrixserverlib.RoomVersion{gomatrixserverlib.RoomVersionV10, gomatrixserverlib.RoomVersionV12} {
		t.Run("room version "+string(version), func(t *testing.T) {
			creator, mod, member, outsider, invited, banned := test.NewUser(t), test.NewUser(t), test.NewUser(t), test.NewUser(t), test.NewUser(t), test.NewUser(t)
			room := test.NewRoom(t, creator, test.RoomVersion(version), test.RoomPreset(test.PresetPublicChat))
			users := map[string]int64{mod.ID: 50}
			if version != gomatrixserverlib.RoomVersionV12 {
				users[creator.ID] = 100
			}
			room.CreateAndInsert(t, creator, spec.MRoomPowerLevels, map[string]any{
				"users": users, "invite": 50, "kick": 50, "ban": 50, "state_default": 50, "events_default": 0,
			}, test.WithStateKey(""))
			for _, u := range []*test.User{mod, member, banned} {
				room.CreateAndInsert(t, u, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(u.ID))
			}
			room.CreateAndInsert(t, mod, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(invited.ID))
			room.CreateAndInsert(t, mod, spec.MRoomMember, map[string]any{"membership": spec.Ban}, test.WithStateKey(banned.ID))

			var state []gomatrixserverlib.PDU
			for _, event := range room.CurrentState() {
				state = append(state, event.PDU)
			}
			provider, err := gomatrixserverlib.NewAuthEvents(state)
			require.NoError(t, err)

			knockProvider, err := gomatrixserverlib.NewAuthEvents(append(state[:len(state):len(state)],
				room.CreateEvent(t, creator, spec.MRoomJoinRules, map[string]any{"join_rule": spec.Knock}, test.WithStateKey("")).PDU))
			require.NoError(t, err)
			memberKey := member.ID
			assert.Equal(t, msc3848AlreadyJoined, msc3848Errcode(knockProvider, spec.SenderID(member.ID), spec.MRoomMember, &memberKey, spec.Knock), "knock by a joined user")

			memberChange := func(sender, target *test.User, membership string) string {
				key := target.ID
				return msc3848Errcode(provider, spec.SenderID(sender.ID), spec.MRoomMember, &key, membership)
			}
			empty := ""
			for _, tc := range []struct {
				name string
				got  string
				want string
			}{
				{"message from a user outside the room", msc3848Errcode(provider, spec.SenderID(outsider.ID), "m.room.message", nil, ""), msc3848NotJoined},
				{"message from an invited user", msc3848Errcode(provider, spec.SenderID(invited.ID), "m.room.message", nil, ""), msc3848NotJoined},
				{"membership event without a state key", msc3848Errcode(provider, spec.SenderID(outsider.ID), spec.MRoomMember, nil, spec.Leave), ""},
				{"message from a member", msc3848Errcode(provider, spec.SenderID(member.ID), "m.room.message", nil, ""), ""},
				{"state event below state_default", msc3848Errcode(provider, spec.SenderID(member.ID), spec.MRoomName, &empty, ""), msc3848InsufficientPower},
				{"state event from a moderator", msc3848Errcode(provider, spec.SenderID(mod.ID), spec.MRoomName, &empty, ""), ""},
				{"third-party invite below the invite level", msc3848Errcode(provider, spec.SenderID(member.ID), spec.MRoomThirdPartyInvite, &empty, ""), msc3848InsufficientPower},

				{"invite of a joined user", memberChange(mod, member, spec.Invite), msc3848AlreadyJoined},
				{"invite by a user outside the room", memberChange(outsider, invited, spec.Invite), msc3848NotJoined},
				{"invite below the invite level", memberChange(member, outsider, spec.Invite), msc3848InsufficientPower},
				{"invite of a banned user", memberChange(mod, banned, spec.Invite), ""},
				{"knock by a joined user in a public room", memberChange(member, member, spec.Knock), ""},
				{"knock by a user outside the room", memberChange(outsider, outsider, spec.Knock), ""},
				{"join", memberChange(outsider, outsider, spec.Join), ""},

				{"kick below the kick level", memberChange(member, invited, spec.Leave), msc3848InsufficientPower},
				{"leaving the room", memberChange(mod, mod, spec.Leave), ""},
				{"kick of the creator", memberChange(mod, creator, spec.Leave), msc3848InsufficientPower},
				{"kick by a user outside the room", memberChange(outsider, member, spec.Leave), msc3848NotJoined},
				{"invited user rejecting the invite", memberChange(invited, invited, spec.Leave), ""},
				{"unban below the ban level", memberChange(member, banned, spec.Leave), msc3848InsufficientPower},
				{"unban by a moderator", memberChange(mod, banned, spec.Leave), ""},
				{"ban below the ban level", memberChange(member, outsider, spec.Ban), msc3848InsufficientPower},
				{"ban of the creator", memberChange(mod, creator, spec.Ban), msc3848InsufficientPower},
				{"ban by a moderator", memberChange(mod, member, spec.Ban), ""},
			} {
				assert.Equal(t, tc.want, tc.got, tc.name)
			}
		})
	}
}

// MSC3848: the errcode is only added with the MSC enabled, and only to
// M_FORBIDDEN errors; errcode itself stays M_FORBIDDEN.
func TestWithMSC3848(t *testing.T) {
	forbidden := util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("nope")}
	enabled := &config.MSCs{MSCs: []string{"msc3848"}}

	body := func(res util.JSONResponse) string {
		b, err := json.Marshal(res.JSON)
		require.NoError(t, err)
		return string(b)
	}
	assert.JSONEq(t, `{"errcode":"M_FORBIDDEN","error":"nope","org.matrix.msc3848.unstable.errcode":"ORG.MATRIX.MSC3848.NOT_JOINED"}`,
		body(withMSC3848(enabled, forbidden, msc3848NotJoined)))
	assert.JSONEq(t, `{"errcode":"M_FORBIDDEN","error":"nope"}`, body(withMSC3848(&config.MSCs{}, forbidden, msc3848NotJoined)))
	assert.JSONEq(t, `{"errcode":"M_FORBIDDEN","error":"nope"}`, body(withMSC3848(nil, forbidden, msc3848NotJoined)))
	assert.JSONEq(t, `{"errcode":"M_FORBIDDEN","error":"nope"}`, body(withMSC3848(enabled, forbidden, "")))
	other := util.JSONResponse{Code: http.StatusForbidden, JSON: spec.GuestAccessForbidden("guests")}
	assert.JSONEq(t, body(other), body(withMSC3848(enabled, other, msc3848NotJoined)))
}
