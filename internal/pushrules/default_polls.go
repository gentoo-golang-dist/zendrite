// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package pushrules

import "slices"

// The MSC3930 poll rules, with the unstable IDs and MSC3381 event types that
// Synapse and current clients use.
const (
	MRulePollResponse      = ".org.matrix.msc3930.rule.poll_response"
	MRulePollStartOneToOne = ".org.matrix.msc3930.rule.poll_start_one_to_one"
	MRulePollStart         = ".org.matrix.msc3930.rule.poll_start"
	MRulePollEndOneToOne   = ".org.matrix.msc3930.rule.poll_end_one_to_one"
	MRulePollEnd           = ".org.matrix.msc3930.rule.poll_end"

	pollStartEventType    = "org.matrix.msc3381.poll.start"
	pollResponseEventType = "org.matrix.msc3381.poll.response"
	pollEndEventType      = "org.matrix.msc3381.poll.end"
)

// mRulePollResponseDefinition keeps poll responses from notifying. It is
// the last default override rule, as in Synapse.
func mRulePollResponseDefinition() *Rule {
	return &Rule{
		RuleID: MRulePollResponse, Default: true, Enabled: true,
		Conditions: []*Condition{{Kind: EventMatchCondition, Key: "type", Pattern: pointer(pollResponseEventType)}},
		Actions:    []*Action{},
	}
}

// defaultPollUnderrideRules returns the MSC3930 underride rules, which come
// after the other default underride rules, in Synapse's order. Each
// one-to-one rule precedes its rule for any room.
func defaultPollUnderrideRules() []*Rule {
	oneToOne := func(ruleID, eventType string) *Rule {
		return &Rule{
			RuleID: ruleID, Default: true, Enabled: true,
			Conditions: []*Condition{
				{Kind: RoomMemberCountCondition, Is: "2"},
				{Kind: EventMatchCondition, Key: "type", Pattern: pointer(eventType)},
			},
			Actions: []*Action{{Kind: NotifyAction}, {Kind: SetTweakAction, Tweak: SoundTweak, Value: "default"}},
		}
	}
	anyRoom := func(ruleID, eventType string) *Rule {
		return &Rule{
			RuleID: ruleID, Default: true, Enabled: true,
			Conditions: []*Condition{{Kind: EventMatchCondition, Key: "type", Pattern: pointer(eventType)}},
			Actions:    []*Action{{Kind: NotifyAction}},
		}
	}
	return []*Rule{
		oneToOne(MRulePollStartOneToOne, pollStartEventType),
		anyRoom(MRulePollStart, pollStartEventType),
		oneToOne(MRulePollEndOneToOne, pollEndEventType),
		anyRoom(MRulePollEnd, pollEndEventType),
	}
}

// addPollRules supplies the MSC3930 rules missing from stored rules after
// the other default rules of their kind, keeping the enablement and actions
// of those already stored.
func addPollRules(rules *RuleSet) {
	insertMissing(&rules.Override, mRulePollResponseDefinition())
	insertMissing(&rules.Underride, defaultPollUnderrideRules()...)
}

// insertMissing adds the given rules, in their order, that rules does not
// hold yet. Each goes before the first stored rule that follows it in
// defaults, or at the end, so that defaults keep their relative order.
func insertMissing(rules *[]*Rule, defaults ...*Rule) {
	index := func(ruleID string) int {
		return slices.IndexFunc(*rules, func(existing *Rule) bool { return existing != nil && existing.RuleID == ruleID })
	}
	for i, rule := range defaults {
		if index(rule.RuleID) >= 0 {
			continue
		}
		at := len(*rules)
		for _, later := range defaults[i+1:] {
			if j := index(later.RuleID); j >= 0 {
				at = j
				break
			}
		}
		*rules = slices.Insert(*rules, at, rule)
	}
}
