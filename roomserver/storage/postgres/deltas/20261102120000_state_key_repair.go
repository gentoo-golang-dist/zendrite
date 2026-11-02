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

// UpStateKeyRepair schedules the one-off repair of state events stored under
// the wrong state key NID. Before #445, auth events fetched over federation
// were stored under the state key of the event they authorized, so for
// example a room's join rules could be filed under a user's ID. The roomserver
// checks every event that can be an auth event once at startup, starting
// from the cursor this inserts, and deletes the row when it has finished.
func UpStateKeyRepair(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO roomserver_state_key_repair (after_event_nid) VALUES (0);`); err != nil {
		return fmt.Errorf("failed to schedule the state key repair: %w", err)
	}
	return nil
}
