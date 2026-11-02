// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package pushrules

import "slices"

// The MSC4306 thread subscription rules, with their unstable IDs.
const (
	MRuleUnsubscribedThread = ".io.element.msc4306.rule.unsubscribed_thread"
	MRuleSubscribedThread   = ".io.element.msc4306.rule.subscribed_thread"
)

// defaultPostContentRules returns the default postcontent rules (MSC4306).
// They run after the mention and keyword rules, so mentions still notify,
// and before the room rules, so a subscribed thread notifies even in a room
// set to mentions only.
func defaultPostContentRules() []*Rule {
	return []*Rule{
		mRuleUnsubscribedThreadDefinition(),
		mRuleSubscribedThreadDefinition(),
	}
}

// mRuleUnsubscribedThreadDefinition keeps events in threads the user is not
// subscribed to from notifying.
func mRuleUnsubscribedThreadDefinition() *Rule {
	return &Rule{
		RuleID:  MRuleUnsubscribedThread,
		Default: true,
		Enabled: true,
		Conditions: []*Condition{
			{Kind: ThreadSubscriptionCondition, Subscribed: pointer(false)},
		},
		Actions: []*Action{},
	}
}

// mRuleSubscribedThreadDefinition notifies for events in threads the user is
// subscribed to.
func mRuleSubscribedThreadDefinition() *Rule {
	return &Rule{
		RuleID:  MRuleSubscribedThread,
		Default: true,
		Enabled: true,
		Conditions: []*Condition{
			{Kind: ThreadSubscriptionCondition, Subscribed: pointer(true)},
		},
		Actions: []*Action{
			{Kind: NotifyAction},
			{Kind: SetTweakAction, Tweak: SoundTweak, Value: "default"},
		},
	}
}

// addPostContentRules supplies the default postcontent rules missing from
// stored rules, keeping the enablement and actions of those already stored.
func addPostContentRules(rules *RuleSet) {
	for _, rule := range defaultPostContentRules() {
		if !slices.ContainsFunc(rules.PostContent, func(existing *Rule) bool { return existing != nil && existing.RuleID == rule.RuleID }) {
			rules.PostContent = append(rules.PostContent, rule)
		}
	}
}
