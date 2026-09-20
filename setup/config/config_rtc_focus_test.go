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
// a flat "livekit_service_url" field on the focus object.
func TestRTCFocus_MarshalJSON_LiveKit(t *testing.T) {
	t.Parallel()

	focus := RTCFocus{
		Type:              "livekit",
		LiveKitServiceURL: "https://matrix.example.com/livekit/jwt",
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

// TestRTCFocus_MarshalJSON_DeprecatedNestedField covers configs predating
// the flat livekit_service_url field: the nested `livekit:` block still
// works, producing the same flat wire shape.
func TestRTCFocus_MarshalJSON_DeprecatedNestedField(t *testing.T) {
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

// TestRTCFocus_MarshalJSON_FlatFieldTakesPrecedence guards the migration
// path: if both the new and deprecated fields are set, the flat one wins.
func TestRTCFocus_MarshalJSON_FlatFieldTakesPrecedence(t *testing.T) {
	t.Parallel()

	focus := RTCFocus{
		Type:              "livekit",
		LiveKitServiceURL: "https://new.example.com/livekit/jwt",
		LiveKit:           &LiveKitFocus{ServiceURL: "https://old.example.com/livekit/jwt"},
	}

	data, err := json.Marshal(focus)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))

	assert.Equal(t, "https://new.example.com/livekit/jwt", got["livekit_service_url"])
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
