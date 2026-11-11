// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/process"
)

// TestPartialStateServerList checks the servers a resync asks for state: the
// join server first, as in Synapse, then the others, never this server.
func TestPartialStateServerList(t *testing.T) {
	cfg := config.FederationAPI{Matrix: &config.Global{
		SigningIdentity: fclient.SigningIdentity{ServerName: "local"},
		VirtualHosts:    []*config.VirtualHost{{SigningIdentity: fclient.SigningIdentity{ServerName: "virtual"}}},
	}}
	w := NewPartialStateWorker(process.NewProcessContext(), nil, &FederationInternalAPI{cfg: &cfg})

	assert.Equal(t,
		[]spec.ServerName{"join", "a", "b"},
		w.buildServerList("join", []string{"a", "local", "join", "virtual", "b"}),
	)
	assert.Equal(t, []spec.ServerName{"a"}, w.buildServerList("", []string{"a"}))
}

// TestPartialStateBackoff checks that the resync's retries back off, within
// their bounds.
func TestPartialStateBackoff(t *testing.T) {
	w := NewPartialStateWorker(process.NewProcessContext(), nil, nil)
	for attempt := range uint32(64) {
		d := w.backoffDuration(attempt)
		assert.Positive(t, d)
		assert.LessOrEqual(t, d, partialStateMaxBackoff)
	}
	assert.Less(t, w.backoffDuration(0), w.backoffDuration(6))
}
