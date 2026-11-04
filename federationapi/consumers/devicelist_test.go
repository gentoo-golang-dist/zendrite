// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

func TestDeviceListUpdates(t *testing.T) {
	keys := userapi.QueryDeviceMessagesResponse{
		StreamID: 7,
		Devices: []userapi.DeviceMessage{{
			StreamID:   5,
			DeviceKeys: &userapi.DeviceKeys{DeviceID: "KEYED", KeyJSON: []byte(`{"keys":{}}`)},
		}},
	}
	updates := deviceListUpdates("@alice:test", []userapi.Device{
		{ID: "KEYED", DisplayName: "phone"},
		{ID: "NOKEYS"},
	}, keys)

	require.Len(t, updates, 2)
	require.Equal(t, "KEYED", updates[0].DeviceID)
	require.Equal(t, "phone", updates[0].DeviceDisplayName)
	require.EqualValues(t, 5, updates[0].StreamID)
	require.JSONEq(t, `{"keys":{}}`, string(updates[0].Keys))
	require.False(t, updates[0].Deleted)

	require.Equal(t, "NOKEYS", updates[1].DeviceID)
	require.EqualValues(t, 7, updates[1].StreamID)
	require.Empty(t, updates[1].Keys)
	require.False(t, updates[1].Deleted, "a device without keys was reported as deleted")
}

func TestWithoutServers(t *testing.T) {
	remaining := withoutServers([]spec.ServerName{"a", "b", "c"}, []spec.ServerName{"b"})
	require.Equal(t, []spec.ServerName{"a", "c"}, remaining)
}
