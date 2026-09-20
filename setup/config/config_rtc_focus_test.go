// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package config

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRTCFocus_MarshalJSON_LiveKit checks the wire shape produced for
// clients against element-hq/lk-jwt-service's documented well-known shape
// (https://github.com/element-hq/lk-jwt-service, "Advertising the service"):
// a flat "livekit_service_url" field on the focus object. The admin-facing
// YAML config nests it under "livekit" for readability - that's a config
// convenience, not a wire contract, so it must not leak into the JSON.
func TestRTCFocus_MarshalJSON_LiveKit(t *testing.T) {
	t.Parallel()

	focus := RTCFocus{
		Type:    "livekit",
		LiveKit: &LiveKitFocus{ServiceURL: "https://matrix.example.com/livekit/jwt"},
	}

	data, err := json.Marshal(focus)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))

	assert.Equal(t, map[string]any{
		"type":                "livekit",
		"livekit_service_url": "https://matrix.example.com/livekit/jwt",
	}, got)
}

func TestRTCFocus_MarshalJSON_NoLiveKitConfig(t *testing.T) {
	t.Parallel()

	focus := RTCFocus{Type: "livekit"}

	data, err := json.Marshal(focus)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))

	assert.Equal(t, map[string]any{"type": "livekit"}, got)
}
