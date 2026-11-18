// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package pushrules

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// The rules as Synapse serves them with msc4075_enabled.
func TestRTCRulesJSON(t *testing.T) {
	t.Parallel()
	rules := DefaultGlobalRuleSet("alice", "example.com")
	for id, want := range map[string]string{
		MRuleRTCInviteForMe: `{"rule_id":".org.matrix.msc4075.rule.rtc.invite_for_me","default":true,"enabled":true,"conditions":[` +
			`{"kind":"event_property_is","key":"type","value":"org.matrix.msc4075.rtc.notification"},` +
			`{"kind":"event_property_contains","key":"content.m\\.mentions.user_ids","value":"@alice:example.com"}],` +
			`"actions":["notify",{"set_tweak":"sound","value":"ring"}]}`,
		MRuleRTCInviteForRoom: `{"rule_id":".org.matrix.msc4075.rule.rtc.invite_for_room","default":true,"enabled":true,"conditions":[` +
			`{"kind":"event_property_is","key":"type","value":"org.matrix.msc4075.rtc.notification"},` +
			`{"kind":"event_property_is","key":"content.m\\.mentions.room","value":true},` +
			`{"kind":"sender_notification_permission","key":"room"}],` +
			`"actions":["notify",{"set_tweak":"sound","value":"ring"}]}`,
	} {
		rule := findRule(rules.Override, id)
		require.NotNil(t, rule, id)
		got, err := json.Marshal(rule)
		require.NoError(t, err)
		require.JSONEq(t, want, string(got))
	}
}

// Both rules come right before the mention rules, as in Synapse.
func TestRTCRulesOrder(t *testing.T) {
	t.Parallel()
	override := ruleIDs(DefaultGlobalRuleSet("alice", "example.com").Override)
	at := slices.Index(override, MRuleRTCInviteForMe)
	require.GreaterOrEqual(t, at, 0)
	require.LessOrEqual(t, at+4, len(override))
	require.Equal(t, []string{MRuleRTCInviteForMe, MRuleRTCInviteForRoom, MRuleIsUserMention, MRuleIsRoomMention}, override[at:at+4])
}

func TestRTCRulesEvaluate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, eventType, content string
		permitted                bool
		want                     string
	}{
		{"mentioned", rtcNotificationEventType, `{"m.mentions":{"user_ids":["@alice:example.com"]}}`, false, MRuleRTCInviteForMe},
		{"someone else", rtcNotificationEventType, `{"m.mentions":{"user_ids":["@bob:example.com"]}}`, true, ""},
		{"room, allowed", rtcNotificationEventType, `{"m.mentions":{"room":true}}`, true, MRuleRTCInviteForRoom},
		{"room, denied", rtcNotificationEventType, `{"m.mentions":{"room":true}}`, false, ""},
		{"a message", "m.room.message", `{"m.mentions":{"user_ids":["@alice:example.com"]}}`, false, MRuleIsUserMention},
		{"room and user", rtcNotificationEventType, `{"m.mentions":{"room":true,"user_ids":["@alice:example.com"]}}`, true, MRuleRTCInviteForMe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := mustEventFromJSON(t, `{"room_id":"!room:example.com","type":"`+tc.eventType+`","content":`+tc.content+`}`)
			rule, err := NewRuleSetEvaluator(mentionContext{fakeEvaluationContext{3}, tc.permitted}, DefaultGlobalRuleSet("alice", "example.com")).MatchEvent(event, UserIDForSender)
			require.NoError(t, err)
			if tc.want == "" {
				require.Nil(t, rule)
				return
			}
			require.NotNil(t, rule)
			require.Equal(t, tc.want, rule.RuleID)
			if tc.eventType == rtcNotificationEventType {
				require.Equal(t, []*Action{{Kind: NotifyAction}, {Kind: SetTweakAction, Tweak: SoundTweak, Value: "ring"}}, rule.Actions)
			}
		})
	}
}

// Stored rules from before MSC4075 get the rules before the mention rules,
// and a user's choice for a stored rule is kept.
func TestUpgradeDefaultRulesAddsRTCRules(t *testing.T) {
	t.Parallel()
	rules := DefaultGlobalRuleSet("alice", "example.com")
	rules.Override = slices.DeleteFunc(rules.Override, func(rule *Rule) bool { return rule.RuleID == MRuleRTCInviteForRoom })
	findRule(rules.Override, MRuleRTCInviteForMe).Enabled = false

	UpgradeDefaultRules(rules, "alice", "example.com")
	require.Equal(t, ruleIDs(DefaultGlobalRuleSet("alice", "example.com").Override), ruleIDs(rules.Override))
	require.False(t, findRule(rules.Override, MRuleRTCInviteForMe).Enabled)
	require.True(t, findRule(rules.Override, MRuleRTCInviteForRoom).Enabled)

	// Without either rule stored.
	rules = DefaultGlobalRuleSet("alice", "example.com")
	rules.Override = slices.DeleteFunc(rules.Override, func(rule *Rule) bool {
		return rule.RuleID == MRuleRTCInviteForMe || rule.RuleID == MRuleRTCInviteForRoom
	})
	UpgradeDefaultRules(rules, "alice", "example.com")
	require.Equal(t, ruleIDs(DefaultGlobalRuleSet("alice", "example.com").Override), ruleIDs(rules.Override))
}
