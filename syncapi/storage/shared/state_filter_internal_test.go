// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared

import "testing"

func TestEventTypeMatches(t *testing.T) {
	for _, tc := range []struct {
		pattern, eventType string
		want               bool
	}{
		{"m.room.member", "m.room.member", true},
		{"m.room.member", "m.room.member2", false},
		{"m.room.*", "m.room.member", true},
		{"m.room.*", "m.space.child", false},
		{"*", "anything", true},
		{"*.member", "m.room.member", true},
		{"m.*.member", "m.room.member", true},
		{"m.*.member", "m.room.name", false},
		{"m.*e*r", "m.room.member", true},
		{"m.room._ember", "m.room.member", false},
	} {
		if got := eventTypeMatches(tc.pattern, tc.eventType); got != tc.want {
			t.Errorf("eventTypeMatches(%q, %q) = %v, want %v", tc.pattern, tc.eventType, got, tc.want)
		}
	}
}
