// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared

import (
	"context"
	"database/sql"
	"fmt"

	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// RejectedRecheckCursor returns the room NID up to which the recheck of
// rejected events has checked rooms, and false if it is not pending.
func (d *Database) RejectedRecheckCursor(ctx context.Context) (types.RoomNID, bool, error) {
	return d.RejectedRecheckTable.SelectRejectedRecheckCursor(ctx, nil)
}

// RoomsWithRejectedEventsAfter returns up to limit rooms after afterRoomNID
// that hold rejected events with state, in NID order.
func (d *Database) RoomsWithRejectedEventsAfter(ctx context.Context, afterRoomNID types.RoomNID, limit int) ([]types.RoomNID, error) {
	roomNIDs, err := d.RejectedRecheckTable.SelectRoomsWithRejectedEventsAfter(ctx, nil, afterRoomNID, limit)
	if err != nil {
		return nil, fmt.Errorf("d.RejectedRecheckTable.SelectRoomsWithRejectedEventsAfter: %w", err)
	}
	return roomNIDs, nil
}

// AdvanceRejectedRecheck records that the recheck has checked rooms up to
// afterRoomNID, or ends it if done is set.
func (d *Database) AdvanceRejectedRecheck(ctx context.Context, afterRoomNID types.RoomNID, done bool) error {
	return d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		if done {
			return d.RejectedRecheckTable.DeleteRejectedRecheck(ctx, txn)
		}
		return d.RejectedRecheckTable.UpdateRejectedRecheckCursor(ctx, txn, afterRoomNID)
	})
}
