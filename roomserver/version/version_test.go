// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package version

import (
	"errors"
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

// The MSC4242 state-DAG room version is known to gomatrixserverlib but not
// implemented here, so it must not count as supported. Every other known
// version stays supported, and RoomVersion still resolves the excluded one.
func TestUnimplementedRoomVersionsAreUnsupported(t *testing.T) {
	stateDAGs := gomatrixserverlib.RoomVersionStateDAGs

	assert.NotContains(t, SupportedRoomVersions(), stateDAGs)
	assert.False(t, IsSupportedRoomVersion(stateDAGs))
	_, err := SupportedRoomVersion(stateDAGs)
	var unsupported UnsupportedVersionError
	require.True(t, errors.As(err, &unsupported), "got %v", err)
	assert.Equal(t, stateDAGs, unsupported.Version)

	impl, err := RoomVersion(stateDAGs)
	require.NoError(t, err)
	assert.Equal(t, stateDAGs, impl.Version())

	EnableMSCs([]string{"msc3757"})
	t.Cleanup(func() { EnableMSCs(nil) })
	for v := range RoomVersions() {
		if v == stateDAGs {
			continue
		}
		assert.Contains(t, SupportedRoomVersions(), v)
		assert.True(t, IsSupportedRoomVersion(v), v)
		_, err = SupportedRoomVersion(v)
		assert.NoError(t, err, v)
	}
	for _, v := range []gomatrixserverlib.RoomVersion{gomatrixserverlib.RoomVersionPseudoIDs, gomatrixserverlib.RoomVersionHydra, gomatrixserverlib.RoomVersionV12} {
		assert.True(t, IsSupportedRoomVersion(v), v)
	}

	_, err = SupportedRoomVersion("org.example.nope")
	var unknown UnknownVersionError
	assert.True(t, errors.As(err, &unknown), "got %v", err)
	assert.False(t, IsSupportedRoomVersion("org.example.nope"))
}

// The MSC3757 owned state room versions are offered only while msc3757 is
// listed in the mscs config.
func TestMSC3757RoomVersionsNeedTheMSC(t *testing.T) {
	versions := []gomatrixserverlib.RoomVersion{gomatrixserverlib.RoomVersionMSC3757V10, gomatrixserverlib.RoomVersionMSC3757V11}
	t.Cleanup(func() { EnableMSCs(nil) })

	for _, mscs := range [][]string{nil, {"msc3391"}} {
		EnableMSCs(mscs)
		for _, v := range versions {
			assert.False(t, IsSupportedRoomVersion(v), v)
			assert.NotContains(t, SupportedRoomVersions(), v)
			_, err := SupportedRoomVersion(v)
			var unsupported UnsupportedVersionError
			assert.True(t, errors.As(err, &unsupported), "got %v", err)
			_, err = RoomVersion(v)
			assert.NoError(t, err, v)
		}
	}

	EnableMSCs([]string{"msc3391", "msc3757"})
	for _, v := range versions {
		assert.True(t, IsSupportedRoomVersion(v), v)
		assert.Contains(t, SupportedRoomVersions(), v)
		impl, err := SupportedRoomVersion(v)
		require.NoError(t, err, v)
		assert.False(t, impl.Stable())
		assert.True(t, impl.OwnedState())
	}
}
