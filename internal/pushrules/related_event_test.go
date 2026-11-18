// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package pushrules

import (
	"encoding/json"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// relatedEvaluationContext is a fakeEvaluationContext that knows the events
// the current event relates to (MSC3664).
type relatedEvaluationContext struct {
	fakeEvaluationContext
	related   map[string]gomatrixserverlib.PDU
	fallbacks map[string]bool
}

func (r relatedEvaluationContext) RelatedEvent(relType string) (gomatrixserverlib.PDU, bool, error) {
	return r.related[relType], r.fallbacks[relType], nil
}

// MSC3664: related_event_match matches against the related event, as in
// Synapse's tests.
func TestRelatedEventMatch(t *testing.T) {
	t.Parallel()
	original := mustEventFromJSON(t, `{"room_id":"!room:a","sender":"@alice:a","type":"m.room.message","content":{"body":"Original message","msgtype":"m.text"}}`)
	reply := mustEventFromJSON(t, `{"room_id":"!room:a","sender":"@bob:a","type":"m.room.message","content":{"body":"reply"}}`)
	ec := relatedEvaluationContext{related: map[string]gomatrixserverlib.PDU{"m.in_reply_to": original, "m.annotation": original}}
	threadFallback := relatedEvaluationContext{
		related:   map[string]gomatrixserverlib.PDU{"m.in_reply_to": original, "m.thread": original},
		fallbacks: map[string]bool{"m.in_reply_to": true},
	}
	yes, no := true, false

	for _, tc := range []struct {
		name string
		ec   EvaluationContext
		cond Condition
		want bool
	}{
		{"reply to the user", ec, Condition{RelType: "m.in_reply_to", Key: "sender", Pattern: pointer("@alice:a")}, true},
		{"reply to someone else", ec, Condition{RelType: "m.in_reply_to", Key: "sender", Pattern: pointer("@carol:a")}, false},
		{"glob on the related content", ec, Condition{RelType: "m.in_reply_to", Key: "content.body", Pattern: pointer("original*")}, true},
		{"word of the related content", ec, Condition{RelType: "m.in_reply_to", Key: "content.body", Pattern: pointer("MESSAGE")}, true},
		{"part of a word of the related content", ec, Condition{RelType: "m.in_reply_to", Key: "content.body", Pattern: pointer("mess")}, false},
		{"sender is matched whole", ec, Condition{RelType: "m.in_reply_to", Key: "sender", Pattern: pointer("@alice")}, false},
		{"sender ignoring case", ec, Condition{RelType: "m.in_reply_to", Key: "sender", Pattern: pointer("@ALICE:A")}, true},
		{"sender glob", ec, Condition{RelType: "m.in_reply_to", Key: "sender", Pattern: pointer("@*:a")}, true},
		{"other relation type", ec, Condition{RelType: "m.annotation", Key: "sender", Pattern: pointer("@alice:a")}, true},
		{"key without a pattern", ec, Condition{RelType: "m.in_reply_to", Key: "sender"}, false},
		{"any event with the relation", ec, Condition{RelType: "m.in_reply_to"}, true},
		{"no event with the relation", ec, Condition{RelType: "m.replace"}, false},
		{"no rel_type", ec, Condition{Key: "sender", Pattern: pointer("@alice:a")}, false},
		{"fallback reply", threadFallback, Condition{RelType: "m.in_reply_to", Key: "sender", Pattern: pointer("@alice:a")}, false},
		{"fallback reply, fallbacks excluded", threadFallback, Condition{RelType: "m.in_reply_to", Key: "sender", Pattern: pointer("@alice:a"), IncludeFallbacks: &no}, false},
		{"fallback reply, fallbacks included", threadFallback, Condition{RelType: "m.in_reply_to", Key: "sender", Pattern: pointer("@alice:a"), IncludeFallbacks: &yes}, true},
		{"thread relation itself", threadFallback, Condition{RelType: "m.thread"}, true},
		{"context without related events", fakeEvaluationContext{}, Condition{RelType: "m.in_reply_to"}, false},
	} {
		tc.cond.Kind = RelatedEventMatchCondition
		got, err := conditionMatches(&tc.cond, reply, tc.ec)
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}
}

// MSC3664: the condition round-trips through JSON with the unstable kind,
// and a rule must give key and pattern together.
func TestRelatedEventMatchJSONAndValidation(t *testing.T) {
	t.Parallel()
	var cond Condition
	require.NoError(t, json.Unmarshal([]byte(`{"kind":"im.nheko.msc3664.related_event_match","rel_type":"m.in_reply_to","include_fallbacks":true,"key":"sender","pattern":"@me:a"}`), &cond))
	assert.Equal(t, RelatedEventMatchCondition, cond.Kind)
	assert.Equal(t, "m.in_reply_to", cond.RelType)
	require.NotNil(t, cond.IncludeFallbacks)
	assert.True(t, *cond.IncludeFallbacks)

	rule := func(cond Condition) *Rule {
		return &Rule{RuleID: "r", Conditions: []*Condition{&cond}, Actions: []*Action{}}
	}
	assert.Empty(t, ValidateRule(OverrideKind, rule(cond)))
	assert.Empty(t, ValidateRule(OverrideKind, rule(Condition{Kind: RelatedEventMatchCondition, RelType: "m.thread"})))
	assert.NotEmpty(t, ValidateRule(OverrideKind, rule(Condition{Kind: RelatedEventMatchCondition, RelType: "m.thread", Key: "sender"})))
	assert.NotEmpty(t, ValidateRule(OverrideKind, rule(Condition{Kind: RelatedEventMatchCondition, RelType: "m.thread", Pattern: pointer("x")})))
	assert.NotEmpty(t, ValidateRule(OverrideKind, rule(Condition{Kind: RelatedEventMatchCondition})))
}

// MSC3664: the reply rule follows .m.rule.member_event, as in Synapse, keeps
// the user's choices, and goes away with the option.
func TestApplyUserOptionsReply(t *testing.T) {
	t.Parallel()
	on := Options{RelatedEventMatch: true}
	const userID = "@alice:a"

	rules := DefaultGlobalRuleSet("alice", "a")
	require.Nil(t, findRule(rules.Override, MRuleReply))
	ApplyUserOptions(rules, on, userID)
	ids := ruleIDs(rules.Override)
	require.Equal(t, []string{MRuleMaster, MRuleSuppressNotices, MRuleInviteForMe, MRuleMemberEvent, MRuleReply, MRuleRTCInviteForMe}, ids[:6])
	got, err := json.Marshal(findRule(rules.Override, MRuleReply))
	require.NoError(t, err)
	require.JSONEq(t, `{"rule_id":".im.nheko.msc3664.reply","default":true,"enabled":true,`+
		`"conditions":[{"kind":"im.nheko.msc3664.related_event_match","rel_type":"m.in_reply_to","key":"sender","pattern":"@alice:a"}],`+
		`"actions":["notify",{"set_tweak":"highlight"},{"set_tweak":"sound","value":"default"}]}`, string(got))

	findRule(rules.Override, MRuleReply).Enabled = false
	ApplyUserOptions(rules, on, userID)
	require.Equal(t, ids, ruleIDs(rules.Override))
	require.False(t, findRule(rules.Override, MRuleReply).Enabled)

	ApplyUserOptions(rules, Options{}, userID)
	require.Nil(t, findRule(rules.Override, MRuleReply))
	require.Equal(t, ruleIDs(DefaultGlobalRuleSet("alice", "a").Override), ruleIDs(rules.Override))

	// Both options apply together.
	ApplyUserOptions(rules, Options{RelatedEventMatch: true, EncryptedEvent: true}, userID)
	require.NotNil(t, findRule(rules.Override, MRuleReply))
	require.NotNil(t, findRule(rules.Override, MRuleEncryptedEvent))

	// The rule notifies a reply to the user, but not one to someone else.
	original := mustEventFromJSON(t, `{"room_id":"!room:a","sender":"@alice:a","type":"m.room.message","content":{"body":"hi"}}`)
	other := mustEventFromJSON(t, `{"room_id":"!room:a","sender":"@carol:a","type":"m.room.message","content":{"body":"hi"}}`)
	reply := mustEventFromJSON(t, `{"room_id":"!room:a","sender":"@bob:a","type":"m.room.message","content":{"body":"reply"}}`)
	userIDForSender := func(_ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
		return spec.NewUserID(string(senderID), true)
	}
	for _, tc := range []struct {
		to   gomatrixserverlib.PDU
		want string
	}{{original, MRuleReply}, {other, ".m.rule.message"}} {
		ec := relatedEvaluationContext{fakeEvaluationContext: fakeEvaluationContext{memberCount: 3}, related: map[string]gomatrixserverlib.PDU{"m.in_reply_to": tc.to}}
		rule, err := NewRuleSetEvaluator(ec, rules).MatchEvent(reply, userIDForSender)
		require.NoError(t, err)
		require.NotNil(t, rule)
		assert.Equal(t, tc.want, rule.RuleID)
	}
}
