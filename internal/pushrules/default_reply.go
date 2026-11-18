// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package pushrules

import "slices"

// MRuleReply is the MSC3664 rule that notifies for replies to the user's
// events, with the unstable ID Synapse uses.
const MRuleReply = ".im.nheko.msc3664.reply"

// mRuleReplyDefinition notifies, noisily and highlighted, for replies to
// events the user sent. Thread events whose reply is only a fallback do not
// count.
func mRuleReplyDefinition(userID string) *Rule {
	return &Rule{
		RuleID: MRuleReply, Default: true, Enabled: true,
		Conditions: []*Condition{{
			Kind:    RelatedEventMatchCondition,
			RelType: "m.in_reply_to",
			Key:     "sender",
			Pattern: pointer(userID),
		}},
		Actions: []*Action{{Kind: NotifyAction}, {Kind: SetTweakAction, Tweak: HighlightTweak}, {Kind: SetTweakAction, Tweak: SoundTweak, Value: "default"}},
	}
}

// ApplyUserOptions is ApplyOptions for the rules of userID, which also adds
// or drops the MSC3664 reply rule. As in Synapse, the reply rule follows
// .m.rule.member_event, before the MatrixRTC and mention rules. A stored
// rule keeps its enablement and actions while it stays on.
func ApplyUserOptions(rules *RuleSet, opts Options, userID string) {
	ApplyOptions(rules, opts)
	isReply := func(rule *Rule) bool { return rule != nil && rule.RuleID == MRuleReply }
	if !opts.RelatedEventMatch {
		rules.Override = slices.DeleteFunc(rules.Override, isReply)
		return
	}
	if slices.ContainsFunc(rules.Override, isReply) {
		return
	}
	index := len(rules.Override)
	for i, rule := range rules.Override {
		if rule == nil || !rule.Default {
			continue
		}
		if rule.RuleID == MRuleMemberEvent {
			index = i + 1
			break
		}
		if rule.RuleID == MRuleRTCInviteForMe || rule.RuleID == MRuleIsUserMention {
			// Without a member event rule, go before the first rule that
			// follows it.
			index = i
			break
		}
	}
	rules.Override = slices.Insert(rules.Override, index, mRuleReplyDefinition(userID))
}
