// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package streams

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

type accountDataSnapshot struct {
	storage.DatabaseTransaction
	changed map[string][]string
}

func (s accountDataSnapshot) GetAccountDataInRange(context.Context, string, types.Range, *synctypes.EventFilter) (map[string][]string, types.StreamPosition, error) {
	return s.changed, 10, nil
}

type accountDataUserAPI struct {
	userapi.SyncUserAPI
	global map[string]json.RawMessage
}

func (u accountDataUserAPI) QueryAccountData(_ context.Context, req *userapi.QueryAccountDataRequest, res *userapi.QueryAccountDataResponse) error {
	res.GlobalAccountData = map[string]json.RawMessage{}
	if data, ok := u.global[req.DataType]; ok && req.RoomID == "" {
		res.GlobalAccountData[req.DataType] = data
	}
	return nil
}

// TestAccountDataSyncReportsDeletedGlobalData checks that a global account
// data type that changed in range but no longer exists, such as the MSC3890
// settings of a deleted device, reaches incremental syncs with empty content
// and is left out of initial syncs, as in Synapse.
func TestAccountDataSyncReportsDeletedGlobalData(t *testing.T) {
	const deleted = "org.matrix.msc3890.local_notification_settings.DEVICE"
	snapshot := accountDataSnapshot{changed: map[string][]string{"": {deleted, "m.direct"}}}
	provider := &AccountDataStreamProvider{userAPI: accountDataUserAPI{
		global: map[string]json.RawMessage{"m.direct": json.RawMessage(`{"@bob:test":["!r:test"]}`)},
	}}

	for name, from := range map[string]types.StreamPosition{"incremental": 5, "initial": 0} {
		t.Run(name, func(t *testing.T) {
			req := &types.SyncRequest{
				Device:   &userapi.Device{UserID: "@alice:test"},
				Response: types.NewResponse(),
				Log:      logrus.NewEntry(logrus.StandardLogger()),
			}
			require.Equal(t, types.StreamPosition(10), provider.IncrementalSync(t.Context(), snapshot, req, from, 10))
			got := map[string]string{}
			for _, ev := range req.Response.AccountData.Events {
				got[ev.Type] = string(ev.Content)
			}
			require.Equal(t, `{"@bob:test":["!r:test"]}`, got["m.direct"])
			content, ok := got[deleted]
			if from == 0 {
				require.False(t, ok, "initial sync must leave deleted account data out")
				return
			}
			require.True(t, ok, "incremental sync must report deleted account data")
			require.JSONEq(t, `{}`, content)
		})
	}
}
