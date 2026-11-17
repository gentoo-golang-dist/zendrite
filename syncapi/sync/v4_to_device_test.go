// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sync

import (
	"encoding/json"
	"fmt"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// TestSlidingToDeviceLimitKeepsRemainder checks that a to-device response cut
// off at limit names the last message it returned as next_batch, so that
// paging with since delivers every queued message once, in order. A
// next_batch past the cut would acknowledge, and so lose, the rest.
func TestSlidingToDeviceLimitKeepsRemainder(t *testing.T) {
	alice := test.NewUser(t)

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		db, err := storage.NewSyncServerDatasource(processCtx.Context(), cm, &cfg.SyncAPI.Database)
		require.NoError(t, err)
		ctx := t.Context()

		const queued = 5
		var latest types.StreamPosition
		for i := range queued {
			latest, err = db.StoreNewSendForDeviceMessage(ctx, alice.ID, "DEVICE", gomatrixserverlib.SendToDeviceEvent{
				Sender:  alice.ID,
				Type:    "m.test",
				Content: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)),
			})
			require.NoError(t, err)
		}

		rp := &RequestPool{}
		toPos := types.StreamingToken{SendToDevicePosition: latest}
		var got []string
		since := ""
		for range queued {
			snapshot, err := db.NewDatabaseSnapshot(ctx)
			require.NoError(t, err)
			res, _, err := rp.processToDeviceExtension(ctx, snapshot, alice.ID, "DEVICE", &types.ToDeviceRequest{Enabled: true, Limit: 2, Since: since}, nil, toPos)
			require.NoError(t, snapshot.Rollback())
			require.NoError(t, err)
			require.LessOrEqual(t, len(res.Events), 2)
			for _, ev := range res.Events {
				got = append(got, string(ev.Content))
			}
			if len(res.Events) == 0 {
				break
			}
			since = res.NextBatch
		}
		want := make([]string, queued)
		for i := range queued {
			want[i] = fmt.Sprintf(`{"n":%d}`, i)
		}
		require.Equal(t, want, got)

		// The connection position stops at the last message returned, so a
		// client that sends no since resumes from there.
		snapshot, err := db.NewDatabaseSnapshot(ctx)
		require.NoError(t, err)
		defer snapshot.Rollback() //nolint:errcheck
		req := &types.SlidingSyncRequest{Extensions: &types.ExtensionRequest{ToDevice: &types.ToDeviceRequest{Enabled: true, Limit: 2}}}
		res, pos, _, err := rp.ProcessExtensions(ctx, snapshot, nil, req, alice.ID, "DEVICE", 0, nil, toPos, nil, nil, nil)
		require.NoError(t, err)
		require.Len(t, res.ToDevice.Events, 2)
		require.Equal(t, res.ToDevice.NextBatch, fmt.Sprint(pos.SendToDevicePosition))
		require.Less(t, pos.SendToDevicePosition, latest)

		// A negative limit falls back to the default rather than failing.
		res, pos, _, err = rp.ProcessExtensions(ctx, snapshot, nil, &types.SlidingSyncRequest{Extensions: &types.ExtensionRequest{ToDevice: &types.ToDeviceRequest{Enabled: true, Limit: -1}}}, alice.ID, "DEVICE", 0, nil, toPos, nil, nil, nil)
		require.NoError(t, err)
		require.Len(t, res.ToDevice.Events, queued)
		require.Equal(t, latest, pos.SendToDevicePosition)
	})
}

func TestToDeviceAckPosition(t *testing.T) {
	t.Parallel()

	enabled := func(since string) *types.SlidingSyncRequest {
		return &types.SlidingSyncRequest{Extensions: &types.ExtensionRequest{ToDevice: &types.ToDeviceRequest{Enabled: true, Since: since}}}
	}
	pos := types.NewSlidingSyncStreamToken(1, types.StreamingToken{SendToDevicePosition: 7})

	require.Equal(t, types.StreamPosition(5), toDeviceAckPosition(enabled("5"), pos), "since wins over pos")
	require.Equal(t, types.StreamPosition(7), toDeviceAckPosition(enabled(""), pos), "pos without since")
	require.Equal(t, types.StreamPosition(0), toDeviceAckPosition(enabled(""), nil), "initial request")
	require.Equal(t, types.StreamPosition(0), toDeviceAckPosition(enabled("bogus"), pos), "invalid since acknowledges nothing")
	require.Equal(t, types.StreamPosition(0), toDeviceAckPosition(&types.SlidingSyncRequest{}, pos), "extension off")
}
