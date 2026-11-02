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

// UpRejectedRecheck schedules the one-off recheck of rejected events. The
// state key repair accepted memberships that state stored under the wrong
// state key had rejected, but the events that depend on them were checked
// only in the pass that accepted them, and against partial state. The
// roomserver checks the rejected events of every room once at startup,
// starting from the cursor this inserts, and deletes the row when it has
// finished.
func UpRejectedRecheck(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO roomserver_rejected_recheck (after_room_nid) VALUES (0);`); err != nil {
		return fmt.Errorf("failed to schedule the rejected event recheck: %w", err)
	}
	return nil
}

// UpRejectedRecheckRedactions schedules the recheck of rejected events once
// more, unless it is still pending. The first recheck left redactions
// rejected, so messages their senders had deleted while their membership was
// rejected were accepted without their redaction.
func UpRejectedRecheckRedactions(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO roomserver_rejected_recheck (after_room_nid)
		SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM roomserver_rejected_recheck);`); err != nil {
		return fmt.Errorf("failed to schedule the rejected redaction recheck: %w", err)
	}
	return nil
}
