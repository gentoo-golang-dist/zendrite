// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package types

import (
	"encoding/json"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
)

func testExtensionResponse(format ExtensionFormat) ExtensionResponse {
	return ExtensionResponse{
		ToDevice: &V4ToDeviceResponse{NextBatch: "7", Events: []gomatrixserverlib.SendToDeviceEvent{{Sender: "@a:test", Type: "m.test", Content: json.RawMessage(`{}`)}}},
		Typing:   &TypingResponse{Rooms: map[string]synctypes.ClientEvent{"!r:test": {Type: "m.typing", Content: []byte(`{"user_ids":["@a:test"]}`)}}},
		Receipts: &ReceiptsResponse{Rooms: map[string]synctypes.ClientEvent{"!r:test": {Type: "m.receipt", Content: []byte(`{"$e":{"m.read":{"@a:test":{"ts":1}}}}`)}}},
		E2EE:     &E2EEResponse{DeviceOneTimeKeysCount: map[string]int{"signed_curve25519": 1}, DeviceUnusedFallbackKeyTypes: []string{}},
		Format:   format,
	}
}

// TestExtensionResponseFormats checks that the org.matrix.simplified_msc3575
// endpoint keeps Synapse's formats while /v4/sync writes those of MSC4508,
// MSC4538 and MSC4551, under the key the request used.
func TestExtensionResponseFormats(t *testing.T) {
	t.Parallel()

	unstable, err := json.Marshal(testExtensionResponse(ExtensionFormat{}))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"to_device": {"next_batch": "7", "events": [{"sender": "@a:test", "type": "m.test", "content": {}}]},
		"typing": {"rooms": {"!r:test": {"type": "m.typing", "content": {"user_ids": ["@a:test"]}}}},
		"receipts": {"rooms": {"!r:test": {"type": "m.receipt", "content": {"$e": {"m.read": {"@a:test": {"ts": 1}}}}}}},
		"e2ee": {"device_one_time_keys_count": {"signed_curve25519": 1}, "device_unused_fallback_key_types": [], "org.matrix.msc2732.device_unused_fallback_key_types": null}
	}`, string(unstable))

	stable, err := json.Marshal(testExtensionResponse(ExtensionFormat{Stable: true, Prefixed: map[string]bool{ExtensionReceipts: true}}))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"to_device": {"next_batch": "7", "messages": [{"sender": "@a:test", "type": "m.test", "content": {}}]},
		"typing": {"rooms": {"!r:test": {"user_ids": ["@a:test"]}}},
		"org.matrix.msc4551.receipts": {"rooms": {"!r:test": {"$e": {"m.read": {"@a:test": {"ts": 1}}}}}},
		"e2ee": {"device_one_time_keys_count": {"signed_curve25519": 1}, "device_unused_fallback_key_types": [], "org.matrix.msc2732.device_unused_fallback_key_types": null}
	}`, string(stable))

	empty, err := json.Marshal(ExtensionResponse{ToDevice: &V4ToDeviceResponse{NextBatch: "1"}, Format: ExtensionFormat{Stable: true}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"to_device": {"next_batch": "1", "messages": []}}`, string(empty))
}

func TestNormalizeStableExtensions(t *testing.T) {
	t.Parallel()

	req := &ExtensionRequest{
		TypingMSC4508:   &TypingRequest{Enabled: true, Lists: []string{"*"}, Rooms: []string{"*", "!r:test"}},
		ToDevice:        &ToDeviceRequest{Enabled: true},
		ToDeviceMSC4538: &ToDeviceRequest{Enabled: false},
		Receipts:        &ReceiptsRequest{Enabled: true, Lists: []string{"*", "all"}},
	}
	format := req.NormalizeStableExtensions()

	assert.True(t, format.Stable)
	assert.Equal(t, map[string]bool{ExtensionTyping: true}, format.Prefixed, "only extensions named with their prefix alone answer under it")
	require.NotNil(t, req.Typing)
	assert.True(t, req.ToDevice.Enabled, "the unprefixed key wins when both are sent")
	assert.Equal(t, []string{}, req.Typing.Lists, `"*" matches no list`)
	assert.Equal(t, []string{"!r:test"}, req.Typing.Rooms)
	assert.Equal(t, []string{"all"}, req.Receipts.Lists)

	// Omitted lists and rooms keep matching everything.
	req = &ExtensionRequest{AccountData: &AccountDataRequest{Enabled: true}}
	req.NormalizeStableExtensions()
	assert.Nil(t, req.AccountData.Lists)
	assert.Nil(t, req.AccountData.Rooms)

	var none *ExtensionRequest
	assert.True(t, none.NormalizeStableExtensions().Stable)

	req = &ExtensionRequest{TypingMSC4508: &TypingRequest{Enabled: true}}
	req.DropUnstableExtensionKeys()
	assert.Nil(t, req.TypingMSC4508)
	assert.Nil(t, req.Typing, "the unstable endpoint does not read prefixed keys")
}
