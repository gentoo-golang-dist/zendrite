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

func TestApplyOptionsEncryptedEvent(t *testing.T) {
	t.Parallel()
	on := Options{EncryptedEvent: true}

	// The defaults do not hold the rule; the option adds it after the master
	// rule, as in Synapse.
	rules := DefaultGlobalRuleSet("alice", "a")
	require.Nil(t, findRule(rules.Override, MRuleEncryptedEvent))
	custom := &Rule{RuleID: "custom", Enabled: true, Conditions: []*Condition{}, Actions: []*Action{}}
	rules.Override = append([]*Rule{custom}, rules.Override...)
	ApplyOptions(rules, on)
	ids := ruleIDs(rules.Override)
	require.Equal(t, []string{"custom", MRuleMaster, MRuleEncryptedEvent, MRuleSuppressNotices}, ids[:4])
	got, err := json.Marshal(findRule(rules.Override, MRuleEncryptedEvent))
	require.NoError(t, err)
	require.JSONEq(t, `{"rule_id":".org.matrix.msc4028.encrypted_event","default":true,"enabled":true,`+
		`"conditions":[{"kind":"event_match","key":"type","pattern":"m.room.encrypted"}],"actions":["notify"]}`, string(got))

	// A user's choice survives later reads, and adding is idempotent.
	findRule(rules.Override, MRuleEncryptedEvent).Enabled = false
	ApplyOptions(rules, on)
	require.Equal(t, ids, ruleIDs(rules.Override))
	require.False(t, findRule(rules.Override, MRuleEncryptedEvent).Enabled)

	// Turning the option off drops the rule.
	ApplyOptions(rules, Options{})
	require.Nil(t, findRule(rules.Override, MRuleEncryptedEvent))
	require.Equal(t, append([]string{"custom"}, ruleIDs(DefaultGlobalRuleSet("alice", "a").Override)...), ruleIDs(rules.Override))

	// Without a master rule it goes before the first default rule.
	noMaster := &RuleSet{Override: []*Rule{custom, mRuleSuppressEditsDefinition()}}
	ApplyOptions(noMaster, on)
	require.Equal(t, []string{"custom", MRuleEncryptedEvent, MRuleSuppressEdits}, ruleIDs(noMaster.Override))
	empty := &RuleSet{}
	ApplyOptions(empty, on)
	require.Equal(t, []string{MRuleEncryptedEvent}, ruleIDs(empty.Override))
}

func TestEncryptedEventRuleEvaluate(t *testing.T) {
	t.Parallel()
	const room = "!room:a"
	pattern := room
	dontNotify := []*Action{{Kind: DontNotifyAction}}
	encrypted := mustEventFromJSON(t, `{"room_id":"!room:a","sender":"@bob:a","type":"m.room.encrypted","content":{}}`)
	match := func(rules *RuleSet) *Rule {
		t.Helper()
		got, err := NewRuleSetEvaluator(fakeEvaluationContext{3}, rules).MatchEvent(encrypted, UserIDForSender)
		require.NoError(t, err)
		require.NotNil(t, got)
		return got
	}

	// Without the option, an encrypted event in a room set to mentions and
	// keywords only does not notify.
	mentionsOnly := func() *RuleSet {
		rules := DefaultGlobalRuleSet("alice", "a")
		rules.Room = []*Rule{{RuleID: room, Enabled: true, Actions: dontNotify}}
		return rules
	}
	require.Equal(t, room, match(mentionsOnly()).RuleID)

	// With it, the event notifies, so the client can decide once it has
	// decrypted it.
	rules := mentionsOnly()
	ApplyOptions(rules, Options{EncryptedEvent: true})
	got := match(rules)
	require.Equal(t, MRuleEncryptedEvent, got.RuleID)
	require.Equal(t, []*Action{{Kind: NotifyAction}}, got.Actions)

	// A muted room, which is a user override rule, still does not notify.
	mute := &Rule{RuleID: room, Enabled: true, Conditions: []*Condition{{Kind: EventMatchCondition, Key: "room_id", Pattern: &pattern}}, Actions: dontNotify}
	rules.Override = append([]*Rule{mute}, rules.Override...)
	require.Same(t, mute, match(rules))

	// Disabled, the default underride rule applies again.
	rules = DefaultGlobalRuleSet("alice", "a")
	ApplyOptions(rules, Options{EncryptedEvent: true})
	findRule(rules.Override, MRuleEncryptedEvent).Enabled = false
	require.Equal(t, MRuleEncrypted, match(rules).RuleID)
}
