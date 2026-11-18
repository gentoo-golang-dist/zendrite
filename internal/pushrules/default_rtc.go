// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package pushrules

import "encoding/json"

// The MSC4075 rules that ring for MatrixRTC call notifications, with the
// unstable IDs and event type that Synapse and Element Call use.
const (
	MRuleRTCInviteForMe   = ".org.matrix.msc4075.rule.rtc.invite_for_me"
	MRuleRTCInviteForRoom = ".org.matrix.msc4075.rule.rtc.invite_for_room"

	rtcNotificationEventType = "org.matrix.msc4075.rtc.notification"
)

func rtcRingActions() []*Action {
	return []*Action{{Kind: NotifyAction}, {Kind: SetTweakAction, Tweak: SoundTweak, Value: "ring"}}
}

func rtcNotificationTypeCondition() *Condition {
	return &Condition{Kind: EventPropertyIsCondition, Key: "type", Value: json.RawMessage(`"` + rtcNotificationEventType + `"`)}
}

// mRuleRTCInviteForMeDefinition rings for a call notification that mentions
// the user. As in Synapse, it and mRuleRTCInviteForRoomDefinition come right
// before .m.rule.is_user_mention.
func mRuleRTCInviteForMeDefinition(userID string) *Rule {
	value, err := json.Marshal(userID)
	if err != nil {
		panic(err)
	}
	return &Rule{
		RuleID: MRuleRTCInviteForMe, Default: true, Enabled: true,
		Conditions: []*Condition{
			rtcNotificationTypeCondition(),
			{Kind: EventPropertyContainsCondition, Key: `content.m\.mentions.user_ids`, Value: value},
		},
		Actions: rtcRingActions(),
	}
}

// mRuleRTCInviteForRoomDefinition rings for a call notification that
// mentions the room, if the sender may notify the room.
func mRuleRTCInviteForRoomDefinition() *Rule {
	return &Rule{
		RuleID: MRuleRTCInviteForRoom, Default: true, Enabled: true,
		Conditions: []*Condition{
			rtcNotificationTypeCondition(),
			{Kind: EventPropertyIsCondition, Key: `content.m\.mentions.room`, Value: json.RawMessage(`true`)},
			{Kind: SenderNotificationPermissionCondition, Key: "room"},
		},
		Actions: rtcRingActions(),
	}
}

// addRTCRules supplies the MSC4075 rules missing from stored rules right
// before .m.rule.is_user_mention, as in the defaults, keeping the enablement
// and actions of those already stored.
func addRTCRules(rules *RuleSet, userID string) {
	insertMissing(&rules.Override, mRuleRTCInviteForMeDefinition(userID), mRuleRTCInviteForRoomDefinition(), mRuleIsUserMentionDefinition(userID))
}
