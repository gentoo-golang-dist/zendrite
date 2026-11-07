package pushrules

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestCopyRoomRules(t *testing.T) {
	const oldRoom, newRoom, otherRoom = "!old:test", "!new:test", "!other:test"
	pattern := func(s string) *string { return &s }
	dontNotify := []*Action{{Kind: DontNotifyAction}}
	roomCond := func(roomID string) []*Condition {
		return []*Condition{{Kind: EventMatchCondition, Key: "room_id", Pattern: pattern(roomID)}}
	}

	rs := RuleSet{
		Override: []*Rule{
			{RuleID: ".m.rule.master", Default: true, Enabled: false, Actions: []*Action{}},
			{RuleID: "mute", Enabled: true, Conditions: roomCond(oldRoom), Actions: dontNotify},
			{RuleID: "disabled", Enabled: false, Conditions: roomCond(oldRoom), Actions: dontNotify},
		},
		Room: []*Rule{
			{RuleID: otherRoom, Enabled: true, Actions: dontNotify},
			{RuleID: oldRoom, Enabled: true, Actions: dontNotify},
		},
		Underride: []*Rule{
			{RuleID: "other", Enabled: true, Conditions: roomCond(otherRoom), Actions: dontNotify},
		},
	}

	if !CopyRoomRules(&rs, oldRoom, newRoom) {
		t.Fatal("CopyRoomRules reported no change")
	}
	want := RuleSet{
		Override: []*Rule{
			{RuleID: ".m.rule.master", Default: true, Enabled: false, Actions: []*Action{}},
			{RuleID: "mute", Enabled: true, Conditions: roomCond(oldRoom), Actions: dontNotify},
			{RuleID: newRoom, Enabled: true, Conditions: roomCond(newRoom), Actions: dontNotify},
			{RuleID: "disabled", Enabled: false, Conditions: roomCond(oldRoom), Actions: dontNotify},
		},
		Room: []*Rule{
			{RuleID: otherRoom, Enabled: true, Actions: dontNotify},
			{RuleID: oldRoom, Enabled: true, Actions: dontNotify},
			{RuleID: newRoom, Enabled: true, Actions: dontNotify},
		},
		Underride: []*Rule{
			{RuleID: "other", Enabled: true, Conditions: roomCond(otherRoom), Actions: dontNotify},
		},
	}
	if diff := cmp.Diff(want, rs); diff != "" {
		t.Errorf("CopyRoomRules: +got -want:\n%s", diff)
	}

	if CopyRoomRules(&rs, oldRoom, newRoom) {
		t.Error("copying again reported a change")
	}
	if diff := cmp.Diff(want, rs); diff != "" {
		t.Errorf("copying again changed the rules: +got -want:\n%s", diff)
	}
}
