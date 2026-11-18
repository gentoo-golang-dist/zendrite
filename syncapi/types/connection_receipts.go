// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package types

// ReceiptKey identifies a receipt that a newer one replaces: one per user,
// receipt type and thread in a room.
type ReceiptKey struct {
	RoomID, Type, UserID, ThreadID string
}

// Key returns the key of the receipt.
func (r OutputReceiptEvent) Key() ReceiptKey {
	return ReceiptKey{RoomID: r.RoomID, Type: r.Type, UserID: r.UserID, ThreadID: r.ThreadID}
}

// ConnectionReceiptState is what a sliding sync connection holds of the
// receipts of some rooms (MSC4551).
type ConnectionReceiptState struct {
	// Latest are the current receipts of the rooms.
	Latest []OutputReceiptEvent
	// Delivered holds the event ID of each receipt the connection was last
	// given.
	Delivered map[ReceiptKey]string
	// InScope holds the rooms that have been in the extension's scope on the
	// connection.
	InScope map[string]bool
}

// ReceiptDelivery is what a sliding sync response gives of receipts
// (MSC4551): the receipts it sends or counts as sent, the rooms in scope,
// and the rooms whose receipts the connection forgets because the user is
// not joined to them.
type ReceiptDelivery struct {
	Receipts    []OutputReceiptEvent
	InScope     []string
	ForgetRooms []string
}

// Empty reports whether the delivery records nothing.
func (d ReceiptDelivery) Empty() bool {
	return len(d.Receipts) == 0 && len(d.InScope) == 0 && len(d.ForgetRooms) == 0
}
