// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// TestDisabledPushersGetNoPushes covers MSC3881: an event pushes only to the
// enabled pushers, and to a disabled one again once it is enabled.
func TestDisabledPushersGetNoPushes(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := mustCreateDatabase(t, dbType) //nolint:contextcheck
		defer closeDB()
		s := &OutputRoomEventConsumer{db: db}

		disabled, enabled := false, true
		upsert := func(pushKey string, on *bool) {
			t.Helper()
			require.NoError(t, db.UpsertPusher(ctx, api.Pusher{
				Kind: api.HTTPKind, AppID: "app", PushKey: pushKey,
				AppDisplayName: "App", DeviceDisplayName: "Device", Language: "en",
				Data:    map[string]any{"url": "https://push.example.com/_matrix/push/v1/notify"},
				Enabled: on, DeviceID: pushKey,
			}, "alice", "test"))
		}
		pushKeys := func() []string {
			t.Helper()
			byURL, _, err := s.localPushDevices(ctx, "alice", "test", nil)
			require.NoError(t, err)
			var keys []string
			for _, byFormat := range byURL {
				for _, devices := range byFormat {
					for _, d := range devices {
						keys = append(keys, d.PushKey)
					}
				}
			}
			return keys
		}

		upsert("phone", nil)
		upsert("laptop", &disabled)
		assert.ElementsMatch(t, []string{"phone"}, pushKeys())

		upsert("phone", &disabled)
		assert.Empty(t, pushKeys())

		upsert("laptop", &enabled)
		assert.ElementsMatch(t, []string{"laptop"}, pushKeys())
	})
}
