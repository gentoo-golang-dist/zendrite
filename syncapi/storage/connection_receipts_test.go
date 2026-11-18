// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package storage_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/syncapi/storage"
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

		receipts := pendingReceipts(t, db, connectionKey)
		require.Len(t, receipts, 1)

		canceled, cancel := context.WithCancelCause(ctx)
		cancel(nil)
		require.Error(t, db.RecordConnectionReceipts(canceled, connectionKey, 1, types.ReceiptDelivery{Receipts: receipts}))
		require.Len(t, pendingReceipts(t, db, connectionKey), 1, "a canceled request records nothing")

		require.NoError(t, db.RecordConnectionReceipts(ctx, connectionKey, 1, types.ReceiptDelivery{Receipts: receipts}))
		require.Empty(t, pendingReceipts(t, db, connectionKey), "a recorded receipt is not delivered again")
	})
}

// MSC4551: the receipts a response delivers stay pending until a request
// carries its position. Using a position acknowledges them and forgets what
// every other response delivered, including a lost retry's sibling, so the
// client gets those receipts again.
func TestConnectionReceiptsAcknowledged(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		db, closeDB := MustCreateDatabase(t, dbType)
		defer closeDB()

		const roomID, newRoomID, userID = "!room:test", "!new:test", "@alice:test"
		connectionKey, err := db.GetOrCreateConnection(ctx, userID, "device", "conn")
		require.NoError(t, err)
		position := func() int64 {
			t.Helper()
			pos, posErr := db.CreateConnectionPosition(ctx, connectionKey)
			require.NoError(t, posErr)
			return pos
		}
		_, err = db.StoreReceipt(ctx, roomID, "m.read", "@bob:test", "$first", "", 1)
		require.NoError(t, err)
		first := position()
		require.NoError(t, db.RecordConnectionReceipts(ctx, connectionKey, first, types.ReceiptDelivery{
			Receipts: pendingReceipts(t, db, connectionKey), InScope: []string{roomID},
		}))
		require.NoError(t, db.DeleteOtherConnectionPositions(ctx, connectionKey, first))

		// The response at the next position is lost.
		_, err = db.StoreReceipt(ctx, roomID, "m.read", "@bob:test", "$second", "", 2)
		require.NoError(t, err)
		_, err = db.StoreReceipt(ctx, roomID, "m.read", "@carol:test", "$second", "", 3)
		require.NoError(t, err)
		lost := position()
		require.NoError(t, db.RecordConnectionReceipts(ctx, connectionKey, lost, types.ReceiptDelivery{
			Receipts: pendingReceipts(t, db, connectionKey), InScope: []string{newRoomID},
		}))
		require.Empty(t, pendingReceipts(t, db, connectionKey))
		require.True(t, receiptState(t, db, connectionKey, newRoomID).InScope[newRoomID])

		// The client retries with the first position.
		require.NoError(t, db.DeleteOtherConnectionPositions(ctx, connectionKey, first))
		var again []string
		for _, receipt := range pendingReceipts(t, db, connectionKey) {
			again = append(again, receipt.UserID+" "+receipt.EventID)
		}
		require.ElementsMatch(t, []string{"@bob:test $second", "@carol:test $second"}, again)
		state := receiptState(t, db, connectionKey, roomID)
		require.True(t, state.InScope[roomID], "the room entered scope at the first position")
		require.False(t, state.InScope[newRoomID], "the room that entered scope in the lost response enters it again")

		// Two responses to the same position: the client uses the second, so
		// what the first delivered, recorded after the retry began, is
		// forgotten too.
		sibling, used := position(), position()
		require.NoError(t, db.RecordConnectionReceipts(ctx, connectionKey, sibling, types.ReceiptDelivery{Receipts: pendingReceipts(t, db, connectionKey)}))
		require.NoError(t, db.RecordConnectionReceipts(ctx, connectionKey, used, types.ReceiptDelivery{InScope: []string{newRoomID}}))
		require.NoError(t, db.DeleteOtherConnectionPositions(ctx, connectionKey, used))
		require.Len(t, pendingReceipts(t, db, connectionKey), 2)
		require.True(t, receiptState(t, db, connectionKey, newRoomID).InScope[newRoomID])

		// A room the user left is forgotten.
		require.NoError(t, db.RecordConnectionReceipts(ctx, connectionKey, position(), types.ReceiptDelivery{ForgetRooms: []string{roomID}}))
		require.False(t, receiptState(t, db, connectionKey, roomID).InScope[roomID])
	})
}

func receiptState(t *testing.T, db storage.Database, connectionKey int64, roomID string) *types.ConnectionReceiptState {
	t.Helper()
	snapshot, err := db.NewDatabaseSnapshot(context.Background())
	require.NoError(t, err)
	defer snapshot.Rollback() //nolint:errcheck
	state, err := snapshot.SelectConnectionReceiptState(context.Background(), connectionKey, []string{roomID})
	require.NoError(t, err)
	return state
}

// pendingReceipts returns the receipts of !room:test the connection has not
// been given.
func pendingReceipts(t *testing.T, db storage.Database, connectionKey int64) []types.OutputReceiptEvent {
	t.Helper()
	state := receiptState(t, db, connectionKey, "!room:test")
	var pending []types.OutputReceiptEvent
	for _, receipt := range state.Latest {
		if eventID, ok := state.Delivered[receipt.Key()]; !ok || eventID != receipt.EventID {
			pending = append(pending, receipt)
		}
	}
	return pending
}
