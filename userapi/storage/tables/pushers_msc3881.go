// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package tables

import (
	"context"
	"database/sql"
)

// AddPusherEnabledDeviceID adds the MSC3881 columns to userapi_pushers:
// whether the pusher sends notifications, true for every existing pusher as
// the MSC asks, and the ID of the device that registered it. Existing pushers
// take the device ID of the session that registered them, when that device
// still exists.
func AddPusherEnabledDeviceID(ctx context.Context, tx *sql.Tx) error {
	for _, stmt := range []string{
		`ALTER TABLE userapi_pushers ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT TRUE;`,
		`ALTER TABLE userapi_pushers ADD COLUMN device_id TEXT;`,
		`UPDATE userapi_pushers SET device_id = (
			SELECT d.device_id FROM userapi_devices d
			WHERE d.session_id = userapi_pushers.session_id
			AND d.localpart = userapi_pushers.localpart
			AND d.server_name = userapi_pushers.server_name
			LIMIT 1
		) WHERE session_id IS NOT NULL;`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}
