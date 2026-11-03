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

// UpExcludeRepairedEvents keeps from the timelines of /sync the events
// written long after they were sent. The roomserver's repairs announced the
// events and memberships they accepted, sent weeks or months before, and the
// sync API wrote them at new stream positions. They became the latest events
// of their rooms, and a client paginating back from them skipped every event
// sent since. An event is excluded if an event written before it is later in
// the room, both by depth and by more than an hour by timestamp, so that a
// single event with a bogus depth or timestamp does not exclude the events
// after it. Excluded events are still returned by /messages, and their state
// changes still apply.
func UpExcludeRepairedEvents(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE syncapi_output_room_events SET exclude_from_sync = TRUE
		WHERE id IN (
			SELECT id FROM (
				SELECT o.id, o.exclude_from_sync, t.topological_position AS topo, o.origin_server_ts AS ts,
					MAX(t.topological_position) OVER w AS prior_topo,
					MAX(o.origin_server_ts) OVER w AS prior_ts
				FROM syncapi_output_room_events AS o
				JOIN syncapi_output_room_events_topology AS t ON t.event_id = o.event_id
				WINDOW w AS (PARTITION BY o.room_id ORDER BY o.id ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING)
			) AS ordered
			WHERE NOT exclude_from_sync AND topo < prior_topo AND ts < prior_ts - 3600000
		);`); err != nil {
		return fmt.Errorf("failed to exclude repaired events from sync: %w", err)
	}
	return nil
}
