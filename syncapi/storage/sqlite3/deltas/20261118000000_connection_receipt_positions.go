// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package deltas

import (
	"context"
	"database/sql"
)

// UpConnectionReceiptPositions records the sliding sync position whose
// response delivered each receipt, so that a retried request gets the
// receipts of a lost response again (MSC4551). Receipts delivered before
// count as acknowledged (position 0). Each room with delivered receipts is
// marked as in the receipts extension's scope, with a row whose receipt type
// is empty.
func UpConnectionReceiptPositions(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, "ALTER TABLE syncapi_sliding_sync_connection_receipts ADD COLUMN connection_position BIGINT NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO syncapi_sliding_sync_connection_receipts"+
		" (connection_key, room_id, receipt_type, user_id, last_delivered_event_id, thread_id, last_delivered_ts, connection_position)"+
		" SELECT DISTINCT connection_key, room_id, '', '', '', '', 0, 0 FROM syncapi_sliding_sync_connection_receipts"+
		" WHERE receipt_type <> ''")
	return err
}
