// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"net/http"
	"slices"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
)

// MSC3848 errcodes, with Synapse's unstable prefix. While the MSC is
// unstable, errcode stays M_FORBIDDEN and the specific errcode is given in
// msc3848ErrcodeField.
const (
	msc3848NotJoined         = "ORG.MATRIX.MSC3848.NOT_JOINED"
	msc3848AlreadyJoined     = "ORG.MATRIX.MSC3848.ALREADY_JOINED"
	msc3848InsufficientPower = "ORG.MATRIX.MSC3848.INSUFFICIENT_POWER"
)

// msc3848Error is an M_FORBIDDEN error that names the specific failure.
type msc3848Error struct {
	spec.MatrixError
	UnstableErrcode string `json:"org.matrix.msc3848.unstable.errcode"`
}

// msc3848Enabled reports whether MSC3848 errcodes are given.
func msc3848Enabled(mscs *config.MSCs) bool {
	return mscs != nil && mscs.Enabled("msc3848")
}

// withMSC3848 adds the MSC3848 errcode to a 403 M_FORBIDDEN response when the
// MSC is enabled and an errcode applies, and returns other responses as they
// are.
func withMSC3848(mscs *config.MSCs, res util.JSONResponse, errcode string) util.JSONResponse {
	if errcode == "" || res.Code != http.StatusForbidden || !msc3848Enabled(mscs) {
		return res
	}
	var matrixErr spec.MatrixError
	switch e := res.JSON.(type) {
	case spec.MatrixError:
		matrixErr = e
	case *spec.MatrixError:
		matrixErr = *e
	default:
		return res
	}
	if matrixErr.ErrCode != spec.ErrorForbidden {
		return res
	}
	res.JSON = msc3848Error{MatrixError: matrixErr, UnstableErrcode: errcode}
	return res
}

// msc3848Errcode works out which MSC3848 errcode describes why the auth rules
// refuse an event with the given sender, type, state key and, for
// m.room.member events, membership, against the given room state. It follows
// the checks Synapse tags, in the same order, and returns "" when none of
// them is the reason.
func msc3848Errcode(
	provider gomatrixserverlib.AuthEventProvider,
	sender spec.SenderID, eventType string, stateKey *string, membership string,
) string {
	createEvent, _ := provider.Create()
	if createEvent == nil {
		return ""
	}
	verImpl, err := gomatrixserverlib.GetRoomVersion(createEvent.Version())
	if err != nil {
		return ""
	}
	powerLevels, err := gomatrixserverlib.NewPowerLevelContentFromAuthEvents(provider, string(createEvent.SenderID()))
	if err != nil {
		return ""
	}
	creators := gomatrixserverlib.CreatorsFromCreateEvent(createEvent)
	level := func(senderID spec.SenderID) int64 {
		if verImpl.PrivilegedCreators() && slices.Contains(creators, string(senderID)) {
			return gomatrixserverlib.CreatorPowerLevel
		}
		return powerLevels.UserLevel(senderID)
	}
	membershipOf := func(senderID spec.SenderID) string {
		event, _ := provider.Member(senderID)
		if event == nil {
			return ""
		}
		m, _ := event.Membership()
		return m
	}
	senderMembership := membershipOf(sender)

	if eventType == spec.MRoomMember && stateKey == nil {
		// A membership event without a state key is malformed.
		return ""
	}
	if eventType != spec.MRoomMember {
		if senderMembership != spec.Join {
			return msc3848NotJoined
		}
		if level(sender) < powerLevels.EventLevel(eventType, stateKey != nil) {
			return msc3848InsufficientPower
		}
		return ""
	}

	target := spec.SenderID(*stateKey)
	targetMembership := membershipOf(target)
	switch membership {
	case spec.Join:
		return ""
	case spec.Knock:
		// As in Synapse, a knock the join rule refuses, or one for another
		// user, is refused before the target's membership is looked at.
		joinRule, err := gomatrixserverlib.NewJoinRuleContentFromAuthEvents(provider)
		if err != nil || (joinRule.JoinRule != spec.Knock && joinRule.JoinRule != spec.KnockRestricted) || target != sender {
			return ""
		}
		if targetMembership == spec.Join {
			return msc3848AlreadyJoined
		}
		return ""
	}
	// An invited or knocking user may leave without being in the room.
	if membership == spec.Leave && target == sender && (senderMembership == spec.Invite || senderMembership == spec.Knock) {
		return ""
	}
	if senderMembership != spec.Join {
		return msc3848NotJoined
	}
	senderLevel := level(sender)
	switch membership {
	case spec.Invite:
		switch {
		case targetMembership == spec.Ban:
			return ""
		case targetMembership == spec.Join:
			return msc3848AlreadyJoined
		case senderLevel < powerLevels.Invite:
			return msc3848InsufficientPower
		}
	case spec.Leave:
		switch {
		case targetMembership == spec.Ban:
			if senderLevel < powerLevels.Ban {
				return msc3848InsufficientPower
			}
		case target != sender:
			if senderLevel < powerLevels.Kick || senderLevel <= level(target) {
				return msc3848InsufficientPower
			}
		}
	case spec.Ban:
		if senderLevel < powerLevels.Ban || senderLevel <= level(target) {
			return msc3848InsufficientPower
		}
	}
	return ""
}

// msc3848MembershipErrcode works out the MSC3848 errcode for a refused
// membership change of target by sender in a room, from the room's current
// state. It only queries the roomserver when MSC3848 is enabled.
func msc3848MembershipErrcode(
	ctx context.Context, mscs *config.MSCs, rsAPI roomserverAPI.ClientRoomserverAPI,
	roomID string, sender, target spec.UserID, membership string,
) string {
	if !msc3848Enabled(mscs) {
		return ""
	}
	validRoomID, err := spec.NewRoomID(roomID)
	if err != nil {
		return ""
	}
	senderID := msc3848SenderID(ctx, rsAPI, *validRoomID, sender)
	targetID := senderID
	if target != sender {
		targetID = msc3848SenderID(ctx, rsAPI, *validRoomID, target)
	}
	targetKey := string(targetID)
	var res roomserverAPI.QueryLatestEventsAndStateResponse
	if err = rsAPI.QueryLatestEventsAndState(ctx, &roomserverAPI.QueryLatestEventsAndStateRequest{
		RoomID: roomID,
		StateToFetch: []gomatrixserverlib.StateKeyTuple{
			{EventType: spec.MRoomCreate, StateKey: ""},
			{EventType: spec.MRoomPowerLevels, StateKey: ""},
			{EventType: spec.MRoomJoinRules, StateKey: ""},
			{EventType: spec.MRoomMember, StateKey: string(senderID)},
			{EventType: spec.MRoomMember, StateKey: targetKey},
		},
	}, &res); err != nil || !res.RoomExists {
		return ""
	}
	events := make([]gomatrixserverlib.PDU, 0, len(res.StateEvents))
	for _, event := range res.StateEvents {
		events = append(events, event.PDU)
	}
	provider, err := gomatrixserverlib.NewAuthEvents(events)
	if err != nil {
		return ""
	}
	return msc3848Errcode(provider, senderID, spec.MRoomMember, &targetKey, membership)
}

// msc3848SenderID is the sender ID of a user in a room, which is the user ID
// except in rooms with pseudo IDs.
func msc3848SenderID(ctx context.Context, rsAPI roomserverAPI.ClientRoomserverAPI, roomID spec.RoomID, userID spec.UserID) spec.SenderID {
	if senderID, err := rsAPI.QuerySenderIDForUser(ctx, roomID, userID); err == nil && senderID != nil {
		return *senderID
	}
	return spec.SenderIDFromUserID(userID)
}

// msc3848MembershipErrcodeForUsers is msc3848MembershipErrcode for user IDs
// given as strings, returning "" for invalid ones.
func msc3848MembershipErrcodeForUsers(
	ctx context.Context, mscs *config.MSCs, rsAPI roomserverAPI.ClientRoomserverAPI,
	roomID, sender, target, membership string,
) string {
	senderID, err := spec.NewUserID(sender, true)
	if err != nil {
		return ""
	}
	targetID, err := spec.NewUserID(target, true)
	if err != nil {
		return ""
	}
	return msc3848MembershipErrcode(ctx, mscs, rsAPI, roomID, *senderID, *targetID, membership)
}
