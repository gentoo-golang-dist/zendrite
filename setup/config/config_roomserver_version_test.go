// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package config

import (
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/stretchr/testify/require"
)

// default_room_version may not name a room version the server does not
// implement, such as the MSC4242 state-DAG version.
func TestRoomServerDefaultRoomVersionMustBeSupported(t *testing.T) {
	for ver, wantErr := range map[gomatrixserverlib.RoomVersion]bool{
		gomatrixserverlib.RoomVersionV12:       false,
		gomatrixserverlib.RoomVersionHydra:     false,
		gomatrixserverlib.RoomVersionStateDAGs: true,
		"org.example.nope":                     true,
	} {
		var c RoomServer
		c.Defaults(DefaultOpts{})
		c.Matrix = &Global{DatabaseOptions: DatabaseOptions{ConnectionString: "file:test.db"}}
		c.DefaultRoomVersion = ver
		errs := ConfigErrors{}
		c.Verify(&errs)
		if wantErr {
			require.Len(t, errs, 1, ver)
			require.Contains(t, errs[0], "default_room_version", ver)
		} else {
			require.Empty(t, errs, ver)
		}
	}
}
