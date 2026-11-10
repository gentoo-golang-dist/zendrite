// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package pushrules

import "slices"

// MRuleEncryptedEvent is the MSC4028 rule, with the unstable ID Synapse uses.
const MRuleEncryptedEvent = ".org.matrix.msc4028.encrypted_event"

// Options selects the default rules that depend on server configuration.
type Options struct {
	// EncryptedEvent adds the MSC4028 rule, which notifies for every
	// encrypted event so that clients decide after decrypting it.
	EncryptedEvent bool
}

// mRuleEncryptedEventDefinition notifies for every encrypted event (MSC4028).
// Rules the user added, such as those muting a room, still take precedence.
func mRuleEncryptedEventDefinition() *Rule {
	return &Rule{
		RuleID: MRuleEncryptedEvent, Default: true, Enabled: true,
		Conditions: []*Condition{{Kind: EventMatchCondition, Key: "type", Pattern: pointer("m.room.encrypted")}},
		Actions:    []*Action{{Kind: NotifyAction}},
	}
}

// ApplyOptions adds the default rules that opts turns on and drops those it
// turns off. As in Synapse, the MSC4028 rule follows .m.rule.master, before
// the other default override rules. A stored rule keeps its enablement and
// actions while it stays on.
func ApplyOptions(rules *RuleSet, opts Options) {
	isEncryptedEvent := func(rule *Rule) bool { return rule != nil && rule.RuleID == MRuleEncryptedEvent }
	if !opts.EncryptedEvent {
		rules.Override = slices.DeleteFunc(rules.Override, isEncryptedEvent)
		return
	}
	if slices.ContainsFunc(rules.Override, isEncryptedEvent) {
		return
	}
	index := len(rules.Override)
	for i, rule := range rules.Override {
		if rule == nil || !rule.Default {
			continue
		}
		if rule.RuleID == MRuleMaster {
			index = i + 1
			break
		}
		if index == len(rules.Override) {
			// Without a master rule, go before the first default rule.
			index = i
		}
	}
	rules.Override = slices.Insert(rules.Override, index, mRuleEncryptedEventDefinition())
}
