// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package version

import (
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSupportedRoomVersionMSC3389 checks that the MSC3389 room version can be
// used to create rooms and is offered as unstable.
func TestSupportedRoomVersionMSC3389(t *testing.T) {
	impl, err := SupportedRoomVersion("org.matrix.msc3389.10")
	require.NoError(t, err)
	assert.Equal(t, gomatrixserverlib.RoomVersionMSC3389, impl.Version())
	assert.False(t, impl.Stable())
	_, ok := SupportedRoomVersions()[gomatrixserverlib.RoomVersionMSC3389]
	assert.True(t, ok)
}
