package pushrules

import (
	"slices"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
)

// DefaultAccountRuleSets is the complete set of default push rules
// for an account.
func DefaultAccountRuleSets(localpart string, serverName spec.ServerName) *AccountRuleSets {
	return &AccountRuleSets{
		Global: *DefaultGlobalRuleSet(localpart, serverName),
	}
}

// DefaultGlobalRuleSet returns the default ruleset for a given (fully
// qualified) MXID.
func DefaultGlobalRuleSet(localpart string, serverName spec.ServerName) *RuleSet {
	return &RuleSet{
		Override:  defaultOverrideRules("@" + localpart + ":" + string(serverName)),
		Underride: defaultUnderrideRules,
	}
}

// UpgradeDefaultRules brings stored rules in line with the current defaults:
// it supplies default rules introduced after the stored rules were created and
// drops the legacy mention rules removed by MSC4210. Existing rules, actions,
// enablement and custom ordering are otherwise left intact.
func UpgradeDefaultRules(rules *RuleSet, localpart string, serverName spec.ServerName) {
	// The mention rules inherit the legacy rules' preferences, so they must
	// be added before the legacy rules are dropped.
	AddMentionRules(rules, localpart, serverName)
	if !slices.ContainsFunc(rules.Override, func(rule *Rule) bool { return rule.RuleID == MRuleSuppressEdits }) {
		// The spec places it after every other default override rule, and
		// user rules of the same kind take precedence over server defaults.
		rules.Override = append(rules.Override, mRuleSuppressEditsDefinition())
	}
	RemoveLegacyMentionRules(rules)
}

// Legacy mention rules, removed from the predefined rules by MSC4210 (spec
// v1.17). Stored rule sets may still contain them.
const (
	MRuleContainsDisplayName = ".m.rule.contains_display_name"
	MRuleContainsUserName    = ".m.rule.contains_user_name"
	MRuleRoomNotif           = ".m.rule.roomnotif"
)

// RemoveLegacyMentionRules drops the .m.rule.contains_display_name,
// .m.rule.contains_user_name and .m.rule.roomnotif rules, which MSC4210
// (spec v1.17) removed from the predefined rules.
func RemoveLegacyMentionRules(rules *RuleSet) {
	rules.Override = slices.DeleteFunc(rules.Override, func(rule *Rule) bool {
		return rule != nil && (rule.RuleID == MRuleContainsDisplayName || rule.RuleID == MRuleRoomNotif)
	})
	rules.Content = slices.DeleteFunc(rules.Content, func(rule *Rule) bool {
		return rule != nil && rule.RuleID == MRuleContainsUserName
	})
}

// AddMentionRules supplies newly introduced defaults when reading stored rules.
// Existing rules, actions, enablement and custom ordering are left intact.
// New rules inherit the corresponding legacy preference on the first upgrade.
func AddMentionRules(rules *RuleSet, localpart string, serverName spec.ServerName) {
	for _, addition := range []struct {
		rule   *Rule
		legacy string
	}{
		{mRuleIsUserMentionDefinition("@" + localpart + ":" + string(serverName)), MRuleContainsDisplayName},
		{mRuleIsRoomMentionDefinition(), MRuleRoomNotif},
	} {
		exists := false
		for _, rule := range rules.Override {
			if rule.RuleID == addition.rule.RuleID {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		index := len(rules.Override)
		for i, rule := range rules.Override {
			if rule.RuleID == addition.legacy {
				addition.rule.Enabled = rule.Enabled
				addition.rule.Actions = rule.Actions
				index = i
				break
			}
			if index == len(rules.Override) && rule.Default && (rule.RuleID == MRuleTombstone || rule.RuleID == MRuleReaction || rule.RuleID == MRuleRoomACLs) {
				index = i
			}
		}
		rules.Override = append(rules.Override, nil)
		copy(rules.Override[index+1:], rules.Override[index:])
		rules.Override[index] = addition.rule
	}
}
