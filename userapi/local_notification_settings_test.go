// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package userapi_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/matrix-org/util"
	"github.com/nats-io/nats.go"

	"codefloe.com/pat-s/zendrite/internal/eventutil"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// accountDataRecorder records the account data types published to the sync API.
type accountDataRecorder struct {
	mu    sync.Mutex
	types []string
}

func (r *accountDataRecorder) PublishMsg(msg *nats.Msg, _ ...nats.PubOpt) (*nats.PubAck, error) {
	if msg.Subject == "client_data" {
		var data eventutil.AccountData
		if err := json.Unmarshal(msg.Data, &data); err == nil {
			r.mu.Lock()
			r.types = append(r.types, data.Type)
			r.mu.Unlock()
		}
	}
	return &nats.PubAck{}, nil
}

func (r *accountDataRecorder) reset() {
	r.mu.Lock()
	r.types = nil
	r.mu.Unlock()
}

func (r *accountDataRecorder) published() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.types...)
}

const localNotificationSettingsPrefix = "org.matrix.msc3890.local_notification_settings."

// TestDeviceDeletionRemovesLocalNotificationSettings checks that deleting
// devices removes their MSC3890 local notification settings, keeps those of
// other devices and other account data, and tells the sync API.
func TestDeviceDeletionRemovesLocalNotificationSettings(t *testing.T) {
	ctx := context.Background()
	const userID = "@alice:test"

	testCases := []struct {
		name string
		// remove deletes devices; it gets all three device IDs.
		remove func(t *testing.T, intAPI api.UserInternalAPI, devices []string)
		// kept is the index of the device whose settings must survive, or -1.
		kept    int
		deleted []int
	}{
		{
			name: "delete one device",
			remove: func(t *testing.T, intAPI api.UserInternalAPI, devices []string) {
				if err := intAPI.PerformDeviceDeletion(ctx, &api.PerformDeviceDeletionRequest{
					UserID: userID, DeviceIDs: []string{devices[1]},
				}, &api.PerformDeviceDeletionResponse{}); err != nil {
					t.Fatal(err)
				}
			},
			kept:    0,
			deleted: []int{1},
		},
		{
			name: "delete all devices except one",
			remove: func(t *testing.T, intAPI api.UserInternalAPI, devices []string) {
				if err := intAPI.PerformDeviceDeletion(ctx, &api.PerformDeviceDeletionRequest{
					UserID: userID, ExceptDeviceID: devices[0],
				}, &api.PerformDeviceDeletionResponse{}); err != nil {
					t.Fatal(err)
				}
			},
			kept:    0,
			deleted: []int{1, 2},
		},
		{
			name: "password change logging out devices",
			remove: func(t *testing.T, intAPI api.UserInternalAPI, _ []string) {
				if err := intAPI.PerformPasswordUpdate(ctx, &api.PerformPasswordUpdateRequest{
					Localpart: "alice", ServerName: "test", Password: "newpassword", LogoutDevices: true,
				}, &api.PerformPasswordUpdateResponse{}); err != nil {
					t.Fatal(err)
				}
			},
			kept:    -1,
			deleted: []int{0, 1, 2},
		},
	}

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				recorder := &accountDataRecorder{}
				intAPI, _, closeDB := MustMakeInternalAPI(t, apiTestOpts{serverName: "test"}, dbType, recorder) //nolint:contextcheck
				defer closeDB()

				if err := intAPI.PerformAccountCreation(ctx, &api.PerformAccountCreationRequest{
					AccountType: api.AccountTypeUser, Localpart: "alice", ServerName: "test", Password: "password",
				}, &api.PerformAccountCreationResponse{}); err != nil {
					t.Fatal(err)
				}

				devices := make([]string, 3)
				for i := range devices {
					res := api.PerformDeviceCreationResponse{}
					deviceID := util.RandomString(8)
					if err := intAPI.PerformDeviceCreation(ctx, &api.PerformDeviceCreationRequest{
						Localpart: "alice", ServerName: "test", DeviceID: &deviceID,
						AccessToken: util.RandomString(16), NoDeviceListUpdate: true,
					}, &res); err != nil {
						t.Fatal(err)
					}
					devices[i] = res.Device.ID
					if err := intAPI.InputAccountData(ctx, &api.InputAccountDataRequest{
						UserID:      userID,
						DataType:    localNotificationSettingsPrefix + devices[i],
						AccountData: json.RawMessage(`{"is_silenced":true}`),
					}, &api.InputAccountDataResponse{}); err != nil {
						t.Fatal(err)
					}
				}
				if err := intAPI.InputAccountData(ctx, &api.InputAccountDataRequest{
					UserID: userID, DataType: "im.vector.setting.breadcrumbs", AccountData: json.RawMessage(`{"recent_rooms":[]}`),
				}, &api.InputAccountDataResponse{}); err != nil {
					t.Fatal(err)
				}
				recorder.reset()

				tc.remove(t, intAPI, devices)

				queryRes := api.QueryAccountDataResponse{}
				if err := intAPI.QueryAccountData(ctx, &api.QueryAccountDataRequest{UserID: userID}, &queryRes); err != nil {
					t.Fatal(err)
				}
				if _, ok := queryRes.GlobalAccountData["im.vector.setting.breadcrumbs"]; !ok {
					t.Fatal("unrelated account data was removed")
				}
				if tc.kept >= 0 {
					if _, ok := queryRes.GlobalAccountData[localNotificationSettingsPrefix+devices[tc.kept]]; !ok {
						t.Fatalf("settings of remaining device %s were removed", devices[tc.kept])
					}
				}
				published := map[string]bool{}
				for _, typ := range recorder.published() {
					published[typ] = true
				}
				for _, i := range tc.deleted {
					dataType := localNotificationSettingsPrefix + devices[i]
					if _, ok := queryRes.GlobalAccountData[dataType]; ok {
						t.Fatalf("settings of deleted device %s still exist", devices[i])
					}
					// The single-type query path must agree.
					single := api.QueryAccountDataResponse{}
					if err := intAPI.QueryAccountData(ctx, &api.QueryAccountDataRequest{UserID: userID, DataType: dataType}, &single); err != nil {
						t.Fatal(err)
					}
					if _, ok := single.GlobalAccountData[dataType]; ok {
						t.Fatalf("settings of deleted device %s returned by type", devices[i])
					}
					if !published[dataType] {
						t.Fatalf("removal of %s was not sent to the sync API, got %v", dataType, recorder.published())
					}
				}
				if tc.kept >= 0 && published[localNotificationSettingsPrefix+devices[tc.kept]] {
					t.Fatal("sync API was told about settings that were not removed")
				}
			})
		}
	})
}
