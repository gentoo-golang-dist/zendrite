// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package storage_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// TestPartialStateDeviceListUpdates checks the device list updates kept while
// rooms have partial state (MSC3902).
func TestPartialStateDeviceListUpdates(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, clean := mustCreateKeyDatabase(t, dbType)
		defer clean()

		require.NoError(t, db.AddPartialStateDeviceListUpdate(ctx, "@derek:remote", "D1"))
		require.NoError(t, db.AddPartialStateDeviceListUpdate(ctx, "@derek:remote", "D2"))
		require.NoError(t, db.AddPartialStateDeviceListUpdate(ctx, "@derek:remote", "D1"))
		require.NoError(t, db.AddPartialStateDeviceListUpdate(ctx, "@elsie:remote", "E1"))

		pending, err := db.PartialStateDeviceListUpdates(ctx)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"D1", "D2"}, pending["@derek:remote"])
		assert.Equal(t, []string{"E1"}, pending["@elsie:remote"])

		require.NoError(t, db.DeletePartialStateDeviceListUpdates(ctx, []string{"@derek:remote"}))
		pending, err = db.PartialStateDeviceListUpdates(ctx)
		require.NoError(t, err)
		assert.Equal(t, map[string][]string{"@elsie:remote": {"E1"}}, pending)

		require.NoError(t, db.DeletePartialStateDeviceListUpdates(ctx, nil))
		pending, err = db.PartialStateDeviceListUpdates(ctx)
		require.NoError(t, err)
		assert.Empty(t, pending)
	})
}

// TestForgetRemoteDeviceList checks that the device list of a user whose
// device list is no longer tracked is dropped, so that it is fetched again.
func TestForgetRemoteDeviceList(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, clean := mustCreateKeyDatabase(t, dbType)
		defer clean()
		userID := "@derek:remote"

		synced, err := db.DeviceListSynced(ctx, userID)
		require.NoError(t, err)
		assert.False(t, synced, "never fetched")

		require.NoError(t, db.StoreRemoteDeviceKeys(ctx, []api.DeviceMessage{{
			Type: api.TypeDeviceKeyUpdate,
			DeviceKeys: &api.DeviceKeys{
				UserID: userID, DeviceID: "D1", KeyJSON: []byte(`{"device_id":"D1","user_id":"@derek:remote"}`),
			},
			StreamID: 1,
		}}, nil))
		require.NoError(t, db.MarkDeviceListStale(ctx, userID, false))
		synced, err = db.DeviceListSynced(ctx, userID)
		require.NoError(t, err)
		assert.True(t, synced)

		require.NoError(t, db.MarkDeviceListStale(ctx, userID, true))
		synced, err = db.DeviceListSynced(ctx, userID)
		require.NoError(t, err)
		assert.False(t, synced, "stale")

		require.NoError(t, db.ForgetRemoteDeviceList(ctx, userID))
		synced, err = db.DeviceListSynced(ctx, userID)
		require.NoError(t, err)
		assert.False(t, synced)
		keys, err := db.DeviceKeysForUser(ctx, userID, nil, true)
		require.NoError(t, err)
		assert.Empty(t, keys)
	})
}
