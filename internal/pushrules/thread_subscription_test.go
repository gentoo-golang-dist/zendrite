// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package pushrules

import (
	"encoding/json"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/stretchr/testify/require"
)

type threadContext struct {
	fakeEvaluationContext
	inThread, subscribed bool
}

func (c threadContext) ThreadSubscription() (inThread, subscribed bool, err error) {
	return c.inThread, c.subscribed, nil
}

func TestThreadSubscriptionRules(t *testing.T) {
	t.Parallel()
	reply := mustEventFromJSON(t, `{"room_id":"!room:a","type":"m.room.message","sender":"@bob:a","content":{"body":"hello keyword","m.relates_to":{"rel_type":"m.thread","event_id":"$root"}}}`)
	mention := mustEventFromJSON(t, `{"room_id":"!room:a","type":"m.room.message","sender":"@bob:a","content":{"body":"hi","m.mentions":{"user_ids":["@alice:a"]},"m.relates_to":{"rel_type":"m.thread","event_id":"$root"}}}`)
	main := mustEventFromJSON(t, `{"room_id":"!room:a","type":"m.room.message","sender":"@bob:a","content":{"body":"hello"}}`)
	muteRoom := &Rule{RuleID: "!room:a", Enabled: true, Actions: []*Action{}}
	keyword := &Rule{RuleID: "keyword", Enabled: true, Pattern: pointer("keyword"), Actions: []*Action{{Kind: NotifyAction}, {Kind: SetTweakAction, Tweak: HighlightTweak}}}

	for _, tc := range []struct {
		name                 string
		inThread, subscribed bool
		event                gomatrixserverlib.PDU
		content, room        []*Rule
		want                 string
	}{
		{name: "unsubscribed thread", inThread: true, want: MRuleUnsubscribedThread},
		{name: "subscribed thread", inThread: true, subscribed: true, want: MRuleSubscribedThread},
		// A subscribed thread notifies even when the room is muted.
		{name: "subscribed thread in muted room", inThread: true, subscribed: true, room: []*Rule{muteRoom}, want: MRuleSubscribedThread},
		{name: "unsubscribed thread in muted room", inThread: true, room: []*Rule{muteRoom}, want: MRuleUnsubscribedThread},
		// Keyword (content) rules and mentions (override) run first.
		{name: "keyword in unsubscribed thread", inThread: true, content: []*Rule{keyword}, want: "keyword"},
		{name: "mention in unsubscribed thread", inThread: true, event: mention, want: MRuleIsUserMention},
		// Outside threads the condition never matches.
		{name: "main timeline", event: main, want: MRuleMessage},
		{name: "main timeline in muted room", event: main, room: []*Rule{muteRoom}, want: "!room:a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rules := DefaultGlobalRuleSet("alice", "a")
			rules.Content = tc.content
			rules.Room = tc.room
			event := tc.event
			if event == nil {
				event = reply
			}
			rule, err := NewRuleSetEvaluator(threadContext{fakeEvaluationContext{3}, tc.inThread, tc.subscribed}, rules).MatchEvent(event, UserIDForSender)
			require.NoError(t, err)
			require.NotNil(t, rule)
			require.Equal(t, tc.want, rule.RuleID)
		})
	}
}

func TestThreadSubscriptionCondition(t *testing.T) {
	t.Parallel()
	event := mustEventFromJSON(t, `{"room_id":"!room:a","type":"m.room.message"}`)
	for _, tc := range []struct {
		name               string
		subscribed         *bool
		inThread, isSubbed bool
		want               bool
	}{
		{name: "missing subscribed", inThread: true, want: false},
		{name: "not in thread", subscribed: pointer(false), want: false},
		{name: "unsubscribed", subscribed: pointer(false), inThread: true, want: true},
		{name: "subscribed wants unsubscribed", subscribed: pointer(false), inThread: true, isSubbed: true, want: false},
		{name: "subscribed", subscribed: pointer(true), inThread: true, isSubbed: true, want: true},
	} {
		got, err := conditionMatches(&Condition{Kind: ThreadSubscriptionCondition, Subscribed: tc.subscribed}, event, threadContext{fakeEvaluationContext{3}, tc.inThread, tc.isSubbed})
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}
}

func TestThreadSubscriptionRulesJSON(t *testing.T) {
	t.Parallel()
	bs, err := json.Marshal(DefaultGlobalRuleSet("alice", "a"))
	require.NoError(t, err)
	var raw struct {
		PostContent []json.RawMessage `json:"postcontent"`
	}
	require.NoError(t, json.Unmarshal(bs, &raw))
	require.Len(t, raw.PostContent, 2)
	require.JSONEq(t, `{"rule_id":".io.element.msc4306.rule.unsubscribed_thread","default":true,"enabled":true,`+
		`"conditions":[{"kind":"io.element.msc4306.thread_subscription","subscribed":false}],"actions":[]}`, string(raw.PostContent[0]))
	require.JSONEq(t, `{"rule_id":".io.element.msc4306.rule.subscribed_thread","default":true,"enabled":true,`+
		`"conditions":[{"kind":"io.element.msc4306.thread_subscription","subscribed":true}],`+
		`"actions":["notify",{"set_tweak":"sound","value":"default"}]}`, string(raw.PostContent[1]))
}

func TestUpgradeDefaultRulesAddsPostContent(t *testing.T) {
	t.Parallel()
	stored := DefaultGlobalRuleSet("alice", "a")
	stored.PostContent = nil
	UpgradeDefaultRules(stored, "alice", "a")
	require.Len(t, stored.PostContent, 2)
	require.Equal(t, MRuleUnsubscribedThread, stored.PostContent[0].RuleID)
	require.Equal(t, MRuleSubscribedThread, stored.PostContent[1].RuleID)

	// A rule the user disabled stays disabled; the missing one is added.
	disabled := mRuleUnsubscribedThreadDefinition()
	disabled.Enabled = false
	stored.PostContent = []*Rule{disabled}
	UpgradeDefaultRules(stored, "alice", "a")
	require.Len(t, stored.PostContent, 2)
	require.False(t, stored.PostContent[0].Enabled)
	require.Equal(t, MRuleSubscribedThread, stored.PostContent[1].RuleID)
}

func TestValidatePostContent(t *testing.T) {
	t.Parallel()
	errs := ValidateRule(PostContentKind, &Rule{RuleID: "mine", Conditions: []*Condition{}, Actions: []*Action{}})
	require.Contains(t, errs, ErrPostContentRule)

	// The condition is allowed in user rules, but needs subscribed.
	require.Empty(t, ValidateRule(OverrideKind, &Rule{RuleID: "mine", Actions: []*Action{}, Conditions: []*Condition{{Kind: ThreadSubscriptionCondition, Subscribed: pointer(true)}}}))
	require.NotEmpty(t, ValidateRule(OverrideKind, &Rule{RuleID: "mine", Actions: []*Action{}, Conditions: []*Condition{{Kind: ThreadSubscriptionCondition}}}))
}
