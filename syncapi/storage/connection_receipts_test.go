// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
)

// RecordConnectionReceipts records the receipts a sliding sync response
// delivers, so later responses on the connection leave them out, and records
// nothing for a request that was canceled.
func TestRecordConnectionReceipts(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		db, closeDB := MustCreateDatabase(t, dbType)
		defer closeDB()

		const roomID, userID = "!room:test", "@alice:test"
		_, err := db.StoreReceipt(ctx, roomID, "m.read", userID, "$read", "", 1)
		require.NoError(t, err)
		connectionKey, err := db.GetOrCreateConnection(ctx, userID, "device", "conn")
		require.NoError(t, err)

		pending := func() []types.OutputReceiptEvent {
			t.Helper()
			snapshot, snapshotErr := db.NewDatabaseSnapshot(ctx)
			require.NoError(t, snapshotErr)
			defer snapshot.Rollback() //nolint:errcheck
			receipts, selectErr := snapshot.SelectLatestUserReceiptsForConnection(ctx, connectionKey, []string{roomID}, userID)
			require.NoError(t, selectErr)
			return receipts
		}
		receipts := pending()
		require.Len(t, receipts, 1)

		canceled, cancel := context.WithCancelCause(ctx)
		cancel(nil)
		require.Error(t, db.RecordConnectionReceipts(canceled, connectionKey, receipts))
		require.Len(t, pending(), 1, "a canceled request records nothing")

		require.NoError(t, db.RecordConnectionReceipts(ctx, connectionKey, receipts))
		require.Empty(t, pending(), "a recorded receipt is not delivered again")
	})
}
