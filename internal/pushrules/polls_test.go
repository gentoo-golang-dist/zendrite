// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package pushrules

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func ruleIDs(rules []*Rule) []string {
	ids := make([]string, 0, len(rules))
	for _, rule := range rules {
		ids = append(ids, rule.RuleID)
	}
	return ids
}

func findRule(rules []*Rule, ruleID string) *Rule {
	for _, rule := range rules {
		if rule.RuleID == ruleID {
			return rule
		}
	}
	return nil
}

// The rules as Synapse serves them, which is what Complement's
// msc3930/TestPollsLocalPushRules checks.
func TestPollRulesJSON(t *testing.T) {
	t.Parallel()
	rules := DefaultGlobalRuleSet("alice", "a")
	for _, tc := range []struct {
		rules []*Rule
		id    string
		want  string
	}{
		{rules.Override, MRulePollResponse, `{"rule_id":".org.matrix.msc3930.rule.poll_response","default":true,"enabled":true,` +
			`"conditions":[{"kind":"event_match","key":"type","pattern":"org.matrix.msc3381.poll.response"}],"actions":[]}`},
		{rules.Underride, MRulePollStartOneToOne, `{"rule_id":".org.matrix.msc3930.rule.poll_start_one_to_one","default":true,"enabled":true,` +
			`"conditions":[{"kind":"room_member_count","is":"2"},{"kind":"event_match","key":"type","pattern":"org.matrix.msc3381.poll.start"}],` +
			`"actions":["notify",{"set_tweak":"sound","value":"default"}]}`},
		{rules.Underride, MRulePollStart, `{"rule_id":".org.matrix.msc3930.rule.poll_start","default":true,"enabled":true,` +
			`"conditions":[{"kind":"event_match","key":"type","pattern":"org.matrix.msc3381.poll.start"}],"actions":["notify"]}`},
		{rules.Underride, MRulePollEndOneToOne, `{"rule_id":".org.matrix.msc3930.rule.poll_end_one_to_one","default":true,"enabled":true,` +
			`"conditions":[{"kind":"room_member_count","is":"2"},{"kind":"event_match","key":"type","pattern":"org.matrix.msc3381.poll.end"}],` +
			`"actions":["notify",{"set_tweak":"sound","value":"default"}]}`},
		{rules.Underride, MRulePollEnd, `{"rule_id":".org.matrix.msc3930.rule.poll_end","default":true,"enabled":true,` +
			`"conditions":[{"kind":"event_match","key":"type","pattern":"org.matrix.msc3381.poll.end"}],"actions":["notify"]}`},
	} {
		rule := findRule(tc.rules, tc.id)
		require.NotNil(t, rule, tc.id)
		got, err := json.Marshal(rule)
		require.NoError(t, err)
		require.JSONEq(t, tc.want, string(got))
	}
}

func TestPollRulesOrder(t *testing.T) {
	t.Parallel()
	rules := DefaultGlobalRuleSet("alice", "a")
	override := ruleIDs(rules.Override)
	require.Equal(t, []string{MRuleSuppressEdits, MRulePollResponse}, override[len(override)-2:])
	require.Equal(t, []string{
		MRuleCall, MRuleRoomOneToOne, MRuleEncryptedRoomOneToOne, MRuleMessage, MRuleEncrypted,
		MRulePollStartOneToOne, MRulePollStart, MRulePollEndOneToOne, MRulePollEnd,
	}, ruleIDs(rules.Underride))
	// The poll rules are built afresh, so changing one rule set leaves the
	// next one alone.
	last := len(rules.Underride) - 1
	rules.Underride[last].Enabled = false
	require.True(t, DefaultGlobalRuleSet("alice", "a").Underride[last].Enabled)
}

func TestPollRulesEvaluate(t *testing.T) {
	t.Parallel()
	rules := DefaultGlobalRuleSet("alice", "a")
	for _, tc := range []struct {
		eventType string
		members   int
		want      string
	}{
		{"org.matrix.msc3381.poll.start", 2, MRulePollStartOneToOne},
		{"org.matrix.msc3381.poll.start", 3, MRulePollStart},
		{"org.matrix.msc3381.poll.end", 2, MRulePollEndOneToOne},
		{"org.matrix.msc3381.poll.end", 5, MRulePollEnd},
		{"org.matrix.msc3381.poll.response", 2, MRulePollResponse},
		{"org.matrix.msc3381.poll.response", 3, MRulePollResponse},
		// The stable MSC3381 types are not matched, as in Synapse.
		{"m.poll.start", 3, ""},
	} {
		ev := mustEventFromJSON(t, `{"room_id":"!room:a","sender":"@bob:a","type":"`+tc.eventType+`","content":{}}`)
		got, err := NewRuleSetEvaluator(fakeEvaluationContext{tc.members}, rules).MatchEvent(ev, UserIDForSender)
		require.NoError(t, err)
		if tc.want == "" {
			require.Nil(t, got, tc.eventType)
			continue
		}
		require.NotNil(t, got, tc.eventType)
		require.Equal(t, tc.want, got.RuleID, "%s with %d members", tc.eventType, tc.members)
	}
}

func TestUpgradeDefaultRulesAddsPollRules(t *testing.T) {
	t.Parallel()
	stored := DefaultGlobalRuleSet("alice", "a")
	isPoll := func(rule *Rule) bool {
		switch rule.RuleID {
		case MRulePollResponse, MRulePollStartOneToOne, MRulePollStart, MRulePollEndOneToOne, MRulePollEnd:
			return true
		}
		return false
	}
	var override, underride []*Rule
	for _, rule := range stored.Override {
		if !isPoll(rule) {
			override = append(override, rule)
		}
	}
	for _, rule := range stored.Underride {
		if !isPoll(rule) {
			underride = append(underride, rule)
		}
	}
	custom := &Rule{RuleID: "custom", Enabled: true, Conditions: []*Condition{}, Actions: []*Action{{Kind: NotifyAction}}}
	// A user disabled the poll start rule before the others were added.
	disabled := findRule(stored.Underride, MRulePollStart)
	disabled.Enabled = false
	stored.Override = override
	stored.Underride = append([]*Rule{custom}, append(underride, disabled)...)

	UpgradeDefaultRules(stored, "alice", "a")
	want := DefaultGlobalRuleSet("alice", "a")
	require.Equal(t, ruleIDs(want.Override), ruleIDs(stored.Override))
	require.Equal(t, append([]string{"custom"}, MRuleCall, MRuleRoomOneToOne, MRuleEncryptedRoomOneToOne, MRuleMessage, MRuleEncrypted,
		MRulePollStartOneToOne, MRulePollStart, MRulePollEndOneToOne, MRulePollEnd), ruleIDs(stored.Underride))
	require.False(t, findRule(stored.Underride, MRulePollStart).Enabled)
	require.True(t, findRule(stored.Underride, MRulePollEnd).Enabled)

	before, err := json.Marshal(stored)
	require.NoError(t, err)
	UpgradeDefaultRules(stored, "alice", "a")
	after, err := json.Marshal(stored)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after), "not idempotent")
}
