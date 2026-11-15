// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

func TestUploadKeysRejectsInvalidDeviceKeys(t *testing.T) {
	const valid = `"user_id":"@alice:local.test","device_id":"DEVICE","algorithms":["m.olm.v1.curve25519-aes-sha2"],"keys":{"ed25519:DEVICE":"key"},"signatures":{"@alice:local.test":{"ed25519:DEVICE":"sig"}}`
	for _, tc := range []struct {
		name       string
		deviceKeys string
		wantErr    bool
	}{
		{name: "absent", deviceKeys: ``},
		{name: "null", deviceKeys: `null`},
		{name: "valid", deviceKeys: `{` + valid + `}`},
		{name: "only ids", deviceKeys: `{"user_id":"@alice:local.test","device_id":"DEVICE"}`, wantErr: true},
		{name: "missing signatures", deviceKeys: `{"user_id":"@alice:local.test","device_id":"DEVICE","algorithms":[],"keys":{}}`, wantErr: true},
		{name: "wrong type", deviceKeys: `{` + valid + `,"algorithms":"nope"}`, wantErr: true},
		{name: "not an object", deviceKeys: `"nope"`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deviceKeys []byte
			if tc.deviceKeys != "" {
				deviceKeys = []byte(tc.deviceKeys)
			}
			err := validateDeviceKeys(deviceKeys)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}

	// An invalid upload never reaches the key API, which is nil here.
	req := httptest.NewRequest(http.MethodPost, "/keys/upload", strings.NewReader(`{"device_keys":{"user_id":"@alice:local.test","device_id":"DEVICE"}}`))
	res := UploadKeys(req, nil, &userapi.Device{UserID: "@alice:local.test", ID: "DEVICE"})
	require.Equal(t, http.StatusBadRequest, res.Code)
}

func TestUploadKeysRejectsMalformedKeyIDs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keyID   string
		wantErr bool
	}{
		{name: "valid", keyID: "signed_curve25519:AAAAHQ"},
		{name: "colon in key id", keyID: "signed_curve25519:AA:HQ"},
		{name: "no colon", keyID: "signed_curve25519", wantErr: true},
		{name: "empty algorithm", keyID: ":AAAAHQ", wantErr: true},
		{name: "empty key id", keyID: "signed_curve25519:", wantErr: true},
		{name: "empty", keyID: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateKeyIDs(map[string]json.RawMessage{tc.keyID: json.RawMessage(`{}`)})
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}

	// A malformed upload never reaches the key API, which is nil here.
	for _, field := range []string{"one_time_keys", "fallback_keys"} {
		t.Run(field, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/keys/upload", strings.NewReader(`{"`+field+`":{"signed_curve25519":{"key":"k"}}}`))
			res := UploadKeys(req, nil, &userapi.Device{UserID: "@alice:local.test", ID: "DEVICE"})
			require.Equal(t, http.StatusBadRequest, res.Code)
		})
	}
}
