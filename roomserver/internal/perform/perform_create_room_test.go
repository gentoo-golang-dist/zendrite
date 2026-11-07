// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package perform

import (
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"

	"codefloe.com/pat-s/zendrite/internal/eventutil"
)

func TestApplyPowerLevelContentOverride(t *testing.T) {
	verImpl := gomatrixserverlib.MustGetRoomVersion(gomatrixserverlib.RoomVersionV12)

	t.Run("override maps replace the defaults", func(t *testing.T) {
		content := eventutil.InitialPowerLevelsContent(verImpl, "@alice:test")
		override := []byte(`{"users_default":100,"users":{"@bob:test":100},"events":{},"notifications":{}}`)
		if err := applyPowerLevelContentOverride(&content, override); err != nil {
			t.Fatal(err)
		}
		if content.UsersDefault != 100 {
			t.Errorf("users_default = %d, want 100", content.UsersDefault)
		}
		if len(content.Events) != 0 {
			t.Errorf("events = %v, want empty", content.Events)
		}
		if len(content.Notifications) != 0 {
			t.Errorf("notifications = %v, want empty", content.Notifications)
		}
		if len(content.Users) != 1 || content.Users["@bob:test"] != 100 {
			t.Errorf("users = %v, want only @bob:test", content.Users)
		}
	})

	t.Run("absent keys keep the defaults", func(t *testing.T) {
		content := eventutil.InitialPowerLevelsContent(verImpl, "@alice:test")
		if err := applyPowerLevelContentOverride(&content, []byte(`{"ban":20}`)); err != nil {
			t.Fatal(err)
		}
		if content.Ban != 20 {
			t.Errorf("ban = %d, want 20", content.Ban)
		}
		if content.Events["m.room.tombstone"] != 150 {
			t.Errorf("events = %v, want the default events", content.Events)
		}
	})

	t.Run("malformed override", func(t *testing.T) {
		content := eventutil.InitialPowerLevelsContent(verImpl, "@alice:test")
		if err := applyPowerLevelContentOverride(&content, []byte(`[]`)); err == nil {
			t.Error("expected an error")
		}
	})
}
