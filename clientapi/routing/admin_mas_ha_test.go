// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	"codefloe.com/pat-s/zendrite/test"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// errConnectionTerminated stands in for the error a pooled connection
// returns once an HA successor has terminated it, or PostgreSQL failed over.
var errConnectionTerminated = errors.New("FATAL: terminating connection due to administrator command (SQLSTATE 57P01)")

// failingMASUserAPI is the real user API with selected calls failing, as
// they do when the owner loses its database connection mid-request.
type failingMASUserAPI struct {
	userapi.ClientUserAPI
	failProfile    bool
	failDehydrated bool
}

func (f *failingMASUserAPI) SetDisplayName(ctx context.Context, localpart string, serverName spec.ServerName, displayName string) (*authtypes.Profile, bool, error) {
	if f.failProfile {
		return nil, false, errConnectionTerminated
	}
	return f.ClientUserAPI.SetDisplayName(ctx, localpart, serverName, displayName)
}

func (f *failingMASUserAPI) SetAvatarURL(ctx context.Context, localpart string, serverName spec.ServerName, avatarURL string) (*authtypes.Profile, bool, error) {
	if f.failProfile {
		return nil, false, errConnectionTerminated
	}
	return f.ClientUserAPI.SetAvatarURL(ctx, localpart, serverName, avatarURL)
}

func (f *failingMASUserAPI) QueryDehydratedDevice(ctx context.Context, req *userapi.QueryDehydratedDeviceRequest, res *userapi.QueryDehydratedDeviceResponse) error {
	if f.failDehydrated {
		return errConnectionTerminated
	}
	return f.ClientUserAPI.QueryDehydratedDevice(ctx, req, res)
}

// MAS retries a provisioning job only when it was not acknowledged, so a
// profile write lost to a takeover must not be answered with success.
func TestMASProvisionUserFailsOnLostProfileWrite(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		_, cfg, userAPI, closeDB := setupMSC3861Stack(t, dbType, nil)
		defer closeDB()
		failing := &failingMASUserAPI{ClientUserAPI: userAPI, failProfile: true}
		for _, body := range []string{
			`{"localpart":"lostprofile","set_displayname":"Lost"}`,
			`{"localpart":"lostprofile","set_avatar_url":"mxc://test/lost"}`,
			`{"localpart":"lostprofile","unset_displayname":true}`,
		} {
			res := MASProvisionUser(test_masAdminRequest(t, http.MethodPost, "/_synapse/mas/provision_user", "test-admin-token", body), &cfg.ClientAPI, failing)
			if res.Code != http.StatusInternalServerError {
				t.Fatalf("%s: expected 500 for a lost profile write, got %d", body, res.Code)
			}
		}

		// MAS retries against the next owner, which applies it.
		failing.failProfile = false
		res := MASProvisionUser(test_masAdminRequest(t, http.MethodPost, "/_synapse/mas/provision_user", "test-admin-token", `{"localpart":"lostprofile","set_displayname":"Lost"}`), &cfg.ClientAPI, failing)
		if res.Code != http.StatusOK {
			t.Fatalf("retried provisioning: expected 200 for the existing account, got %d", res.Code)
		}
		profile, err := userAPI.QueryProfile(t.Context(), "@lostprofile:"+string(cfg.Global.ServerName))
		if err != nil {
			t.Fatal(err)
		}
		if profile.DisplayName != "Lost" {
			t.Fatalf("display name after the retry is %q, want %q", profile.DisplayName, "Lost")
		}
	})
}

// A failed dehydrated device lookup must not leave the device unprotected
// from the reconciliation's deletions.
func TestMASSyncDevicesFailsOnLostDehydratedLookup(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		routers, cfg, userAPI, closeDB := setupMSC3861Stack(t, dbType, nil)
		defer closeDB()
		masAdminCall(t, routers, http.MethodPost, "/_synapse/mas/provision_user", "test-admin-token", `{"localpart":"dehydrated"}`)
		masAdminCall(t, routers, http.MethodPost, "/_synapse/mas/upsert_device", "test-admin-token", `{"localpart":"dehydrated","device_id":"KEEP1"}`)
		masAdminCall(t, routers, http.MethodPost, "/_synapse/mas/upsert_device", "test-admin-token", `{"localpart":"dehydrated","device_id":"OTHER"}`)

		failing := &failingMASUserAPI{ClientUserAPI: userAPI, failDehydrated: true}
		res := MASSyncDevices(test_masAdminRequest(t, http.MethodPost, "/_synapse/mas/sync_devices", "test-admin-token", `{"localpart":"dehydrated","devices":["KEEP1"]}`), &cfg.ClientAPI, failing)
		if res.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500 when the dehydrated device cannot be looked up, got %d", res.Code)
		}
		var devices userapi.QueryDevicesResponse
		if err := userAPI.QueryDevices(t.Context(), &userapi.QueryDevicesRequest{UserID: "@dehydrated:" + string(cfg.Global.ServerName)}, &devices); err != nil {
			t.Fatal(err)
		}
		if len(devices.Devices) != 2 {
			t.Fatalf("a failed sync deleted devices: %+v", devices.Devices)
		}
	})
}
