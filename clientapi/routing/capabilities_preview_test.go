// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

type capabilitiesRoomserverAPI struct {
	roomserverAPI.ClientRoomserverAPI
}

func (capabilitiesRoomserverAPI) DefaultRoomVersion() gomatrixserverlib.RoomVersion {
	return gomatrixserverlib.RoomVersionV12
}

func (capabilitiesRoomserverAPI) AutoForgetOnLeaveEnabled() bool { return false }

// TestCapabilitiesPreviewURL checks the MSC4452 capability under its stable
// and unstable names: enabled only with URL previews on, and never for the
// guests and call guests that preview_url refuses.
func TestCapabilitiesPreviewURL(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		enabled     bool
		accountType userapi.AccountType
		want        bool
	}{
		{name: "user, previews enabled", enabled: true, accountType: userapi.AccountTypeUser, want: true},
		{name: "admin, previews enabled", enabled: true, accountType: userapi.AccountTypeAdmin, want: true},
		{name: "user, previews disabled", enabled: false, accountType: userapi.AccountTypeUser, want: false},
		{name: "guest, previews enabled", enabled: true, accountType: userapi.AccountTypeGuest, want: false},
		{name: "call guest, previews enabled", enabled: true, accountType: userapi.AccountTypeCallGuest, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res := GetCapabilities(capabilitiesRoomserverAPI{}, false, &config.DelayedEvents{}, tc.enabled, &userapi.Device{AccountType: tc.accountType})
			body, ok := res.JSON.(map[string]any)
			require.True(t, ok)
			capabilities, ok := body["capabilities"].(map[string]any)
			require.True(t, ok)
			for _, key := range []string{"m.preview_url", "io.element.msc4452.preview_url"} {
				assert.Equal(t, map[string]bool{"enabled": tc.want}, capabilities[key], key)
			}
		})
	}
}
