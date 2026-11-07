package pushrules

// CopyRoomRules copies the user's enabled rules for oldRoomID to newRoomID,
// as Synapse does when a room is upgraded. It copies the room rule of the
// old room, and override and underride rules with a room_id condition
// matching the old room. As in Synapse, each copy has the new room ID as its
// rule ID, and is placed right after its original. A rule already present
// under that ID is left alone. It reports whether any rule was copied.
func CopyRoomRules(rs *RuleSet, oldRoomID, newRoomID string) bool {
	if oldRoomID == "" || newRoomID == "" || oldRoomID == newRoomID {
		return false
	}
	changed := false
	for _, rules := range []*[]*Rule{&rs.Override, &rs.Room, &rs.Underride} {
		if copyRoomRules(rules, oldRoomID, newRoomID) {
			changed = true
		}
	}
	return changed
}

func copyRoomRules(rules *[]*Rule, oldRoomID, newRoomID string) bool {
	ids := make(map[string]bool, len(*rules))
	for _, rule := range *rules {
		ids[rule.RuleID] = true
	}
	out := make([]*Rule, 0, len(*rules))
	changed := false
	for _, rule := range *rules {
		out = append(out, rule)
		if rule.Default || !rule.Enabled {
			continue
		}
		var cp *Rule
		switch {
		case rule.RuleID == oldRoomID && len(rule.Conditions) == 0:
			cp = cloneRule(rule)
		case hasRoomIDCondition(rule, oldRoomID):
			cp = cloneRule(rule)
			for _, cond := range cp.Conditions {
				if isRoomIDCondition(cond, oldRoomID) {
					pattern := newRoomID
					cond.Pattern = &pattern
				}
			}
		default:
			continue
		}
		cp.RuleID = newRoomID
		if ids[cp.RuleID] {
			continue
		}
		ids[cp.RuleID] = true
		out = append(out, cp)
		changed = true
	}
	if changed {
		*rules = out
	}
	return changed
}

func hasRoomIDCondition(rule *Rule, roomID string) bool {
	for _, cond := range rule.Conditions {
		if isRoomIDCondition(cond, roomID) {
			return true
		}
	}
	return false
}

func isRoomIDCondition(cond *Condition, roomID string) bool {
	return cond != nil && cond.Kind == EventMatchCondition && cond.Key == "room_id" &&
		cond.Pattern != nil && *cond.Pattern == roomID
}

func cloneRule(rule *Rule) *Rule {
	cp := *rule
	if rule.Conditions != nil {
		cp.Conditions = make([]*Condition, len(rule.Conditions))
		for i, cond := range rule.Conditions {
			c := *cond
			cp.Conditions[i] = &c
		}
	}
	return &cp
}
