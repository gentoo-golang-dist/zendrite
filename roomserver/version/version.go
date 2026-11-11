// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package version

import (
	"fmt"

	"codefloe.com/pat-s/gomatrixserverlib"
)

// RoomVersions returns a map of all known room versions to this
// server.
func RoomVersions() map[gomatrixserverlib.RoomVersion]gomatrixserverlib.IRoomVersion {
	return gomatrixserverlib.RoomVersions()
}

// unsupportedRoomVersions lists the room versions gomatrixserverlib knows
// but this server does not implement. They are not advertised in
// /capabilities, cannot be created or upgraded to, and the server refuses to
// join, knock on or be invited into rooms of these versions. Events of these
// versions can still be parsed through RoomVersion.
//
// To support one of these versions, implement it and drop it from this set.
var unsupportedRoomVersions = map[gomatrixserverlib.RoomVersion]struct{}{
	// MSC4242 state DAGs: locally built events never set prev_state_events
	// and nothing derives auth events from the state DAG. Synapse also only
	// enables it behind an experimental flag.
	gomatrixserverlib.RoomVersionStateDAGs: {},
}

// SupportedRoomVersions returns a map of descriptions for room
// versions that are supported by this homeserver.
func SupportedRoomVersions() map[gomatrixserverlib.RoomVersion]gomatrixserverlib.IRoomVersion {
	known := gomatrixserverlib.RoomVersions()
	supported := make(map[gomatrixserverlib.RoomVersion]gomatrixserverlib.IRoomVersion, len(known))
	for v, impl := range known {
		if IsSupportedRoomVersion(v) {
			supported[v] = impl
		}
	}
	return supported
}

// IsSupportedRoomVersion reports whether the room version is known and
// implemented by this server.
func IsSupportedRoomVersion(version gomatrixserverlib.RoomVersion) bool {
	if !gomatrixserverlib.KnownRoomVersion(version) {
		return false
	}
	_, unsupported := unsupportedRoomVersions[version]
	return !unsupported
}

// RoomVersion returns information about a specific room version.
// An UnknownVersionError is returned if the version is not known
// to the server.
func RoomVersion(version gomatrixserverlib.RoomVersion) (gomatrixserverlib.IRoomVersion, error) {
	if version, ok := gomatrixserverlib.RoomVersions()[version]; ok {
		return version, nil
	}
	return nil, UnknownVersionError{version}
}

// SupportedRoomVersion returns information about a specific room
// version. An UnknownVersionError is returned if the version is not
// known to the server, or an UnsupportedVersionError is returned if
// the version is known but specifically marked as unsupported.
func SupportedRoomVersion(version gomatrixserverlib.RoomVersion) (gomatrixserverlib.IRoomVersion, error) {
	impl, err := RoomVersion(version)
	if err != nil {
		return nil, err
	}
	if _, unsupported := unsupportedRoomVersions[version]; unsupported {
		return nil, UnsupportedVersionError{version}
	}
	return impl, nil
}

// UnknownVersionError is caused when the room version is not known.
type UnknownVersionError struct {
	Version gomatrixserverlib.RoomVersion
}

func (e UnknownVersionError) Error() string {
	return fmt.Sprintf("room version '%s' is not known", e.Version)
}

// UnsupportedVersionError is caused when the room version is specifically
// marked as unsupported.
type UnsupportedVersionError struct {
	Version gomatrixserverlib.RoomVersion
}

func (e UnsupportedVersionError) Error() string {
	return fmt.Sprintf("room version '%s' is not supported by this server", e.Version)
}
