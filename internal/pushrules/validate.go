package pushrules

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// ValidateRule checks the rule for errors. These follow from Sytests
// and the specification.
func ValidateRule(kind Kind, rule *Rule) []error {
	var errs []error

	if len(rule.RuleID) > 0 && rule.RuleID[:1] == "." {
		errs = append(errs, fmt.Errorf("invalid rule ID: rule can not start with a dot"))
	}

	if !validRuleIDRE.MatchString(rule.RuleID) {
		errs = append(errs, fmt.Errorf("invalid rule ID: %s", rule.RuleID))
	}

	if rule.Actions == nil {
		errs = append(errs, fmt.Errorf("missing actions"))
	}
	for _, action := range rule.Actions {
		errs = append(errs, validateAction(action)...)
	}

	for _, cond := range rule.Conditions {
		errs = append(errs, validateCondition(cond)...)
	}

	switch kind {
	case OverrideKind, UnderrideKind:
		// The empty list is allowed, but for JSON-encoding reasons,
		// it must not be nil.
		if rule.Conditions == nil {
			errs = append(errs, fmt.Errorf("missing rule conditions"))
		}

	case ContentKind:
		if rule.Pattern == nil {
			errs = append(errs, fmt.Errorf("missing content rule pattern"))
		}
		if rule.Pattern != nil && *rule.Pattern == "" {
			errs = append(errs, fmt.Errorf("missing content rule pattern"))
		}

	case RoomKind, SenderKind:
		// Do nothing.

	case PostContentKind:
		// MSC4306: clients cannot create rules of this kind for now.
		errs = append(errs, ErrPostContentRule)

	default:
		errs = append(errs, fmt.Errorf("invalid rule kind: %s", kind))
	}

	return errs
}

// ErrPostContentRule is returned for client rules of the postcontent kind,
// which MSC4306 reserves for server-default rules.
var ErrPostContentRule = fmt.Errorf("user-defined rules using `postcontent` are not accepted")

// validRuleIDRE is a regexp for valid IDs.
//
// TODO: the specification doesn't seem to say what the rule ID syntax
// is. A Sytest fails if it contains a backslash.
var validRuleIDRE = regexp.MustCompile(`^([^\\]+)$`)

// validateAction returns issues with an Action.
func validateAction(action *Action) []error {
	var errs []error

	switch action.Kind {
	case NotifyAction, DontNotifyAction, CoalesceAction, SetTweakAction:
		// Do nothing.

	default:
		errs = append(errs, fmt.Errorf("invalid rule action kind: %s", action.Kind))
	}

	return errs
}

// validateCondition returns issues with a Condition.
func validateCondition(cond *Condition) []error {
	var errs []error

	switch cond.Kind {
	case EventPropertyIsCondition, EventPropertyContainsCondition:
		var value any
		if cond.Key == "" {
			errs = append(errs, fmt.Errorf("missing condition key"))
		}
		if len(cond.Value) == 0 || json.Unmarshal(cond.Value, &value) != nil || !scalarValue(value) {
			errs = append(errs, fmt.Errorf("invalid condition value"))
		}
	case EventMatchCondition, ContainsDisplayNameCondition, RoomMemberCountCondition, SenderNotificationPermissionCondition:
		// Do nothing.
	case ThreadSubscriptionCondition:
		if cond.Subscribed == nil {
			errs = append(errs, fmt.Errorf("missing condition subscribed"))
		}

	default:
		errs = append(errs, fmt.Errorf("invalid rule condition kind: %s", cond.Kind))
	}

	return errs
}
