// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package deltas

import (
	"context"
	"database/sql"
	"fmt"
)

// UpResyncRepairPending adds a resync_repair_pending column to
// roomserver_rooms and sets it for every room that already completed a
// partial-state resync. Those resyncs replaced only the current state and left
// the forward extremities with partial state, so the roomserver repairs them
// once at startup and clears the flag.
func UpResyncRepairPending(ctx context.Context, tx *sql.Tx) error {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('roomserver_rooms') WHERE name = 'resync_repair_pending'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check column existence: %w", err)
	}
	if count == 0 {
		if _, err = tx.ExecContext(ctx, `ALTER TABLE roomserver_rooms ADD COLUMN resync_repair_pending BOOLEAN NOT NULL DEFAULT FALSE;`); err != nil {
			return fmt.Errorf("failed to add resync_repair_pending: %w", err)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE roomserver_rooms SET resync_repair_pending = TRUE WHERE resync_state_nid > 0;`); err != nil {
		return fmt.Errorf("failed to flag resynced rooms: %w", err)
	}
	return nil
}
