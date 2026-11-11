// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package caching

import "codefloe.com/pat-s/gomatrixserverlib/spec"

// RemoteProfileFailureCache remembers remote servers whose federation profile
// query recently failed without an HTTP response (unreachable, timed out), so
// that client profile lookups for their users fail fast instead of dialing out.
type RemoteProfileFailureCache interface {
	// RemoteProfileFailed returns true if a profile query to the server failed recently.
	RemoteProfileFailed(serverName spec.ServerName) bool
	// StoreRemoteProfileFailure marks the server's profile queries as failing.
	StoreRemoteProfileFailure(serverName spec.ServerName)
}

func (c Caches) RemoteProfileFailed(serverName spec.ServerName) bool {
	_, ok := c.RemoteProfileFailures.Get(string(serverName))
	return ok
}

func (c Caches) StoreRemoteProfileFailure(serverName spec.ServerName) {
	c.RemoteProfileFailures.Set(string(serverName), struct{}{})
}
