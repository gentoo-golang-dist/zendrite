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

// UpRejectedRecheckRejoins schedules the recheck of rejected events once
// more, unless it is still pending. The earlier rechecks left the events of a
// member rejected when their rejoin was missing from the state and the join
// they cited was a later profile change, which is only allowed after it.
func UpRejectedRecheckRejoins(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO roomserver_rejected_recheck (after_room_nid)
		SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM roomserver_rejected_recheck);`); err != nil {
		return fmt.Errorf("failed to schedule the rejected event recheck after rejoins: %w", err)
	}
	return nil
}

// UpRejectedRecheckStaleMemberships schedules the recheck of rejected events
// once more, unless it is still pending. The recheck now also adds to the
// current state the memberships stored as outliers that follow the one it
// holds for their user.
func UpRejectedRecheckStaleMemberships(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO roomserver_rejected_recheck (after_room_nid)
		SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM roomserver_rejected_recheck);`); err != nil {
		return fmt.Errorf("failed to schedule the stale membership repair: %w", err)
	}
	return nil
}

// UpRejectedRecheckRemoteMemberships schedules the recheck of rejected
// events once more, unless it is still pending. The recheck now also takes
// later memberships it cannot add itself from the room's state on other
// servers, and picks the latest of equal memberships that follow the current
// one.
func UpRejectedRecheckRemoteMemberships(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO roomserver_rejected_recheck (after_room_nid)
		SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM roomserver_rejected_recheck);`); err != nil {
		return fmt.Errorf("failed to schedule the remote membership repair: %w", err)
	}
	return nil
}

// UpRejectedRecheckMembershipVotes schedules the recheck of rejected events
// once more, unless it is still pending. The remote membership repair now
// logs the membership each server holds for the users it leaves alone.
func UpRejectedRecheckMembershipVotes(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO roomserver_rejected_recheck (after_room_nid)
		SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM roomserver_rejected_recheck);`); err != nil {
		return fmt.Errorf("failed to schedule the membership repair with logged votes: %w", err)
	}
	return nil
}

// UpRejectedRecheckForkedMemberships schedules the recheck of rejected events
// once more, unless it is still pending. The local membership repair dropped
// the users whose memberships fork when it had nothing else to add in their
// room, so the remote repair never asked about them.
func UpRejectedRecheckForkedMemberships(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO roomserver_rejected_recheck (after_room_nid)
		SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM roomserver_rejected_recheck);`); err != nil {
		return fmt.Errorf("failed to schedule the forked membership repair: %w", err)
	}
	return nil
}

// UpRepairStrayEvents moves events stored under a room NID that no longer
// exists to the room their JSON names, and schedules the recheck of rejected
// events once more, unless it is still pending. A NID cached from a
// transaction that was rolled back stored the create event of a room under a
// room that never existed, so every event of the room cited an auth event the
// recheck could not find in it. A create event of a room version that derives
// the room ID from it names no room, so its event ID gives the room ID.
func UpRepairStrayEvents(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, repairStrayEventsSQL); err != nil {
		return fmt.Errorf("failed to move events stored under a room that does not exist: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO roomserver_rejected_recheck (after_room_nid)
		SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM roomserver_rejected_recheck);`); err != nil {
		return fmt.Errorf("failed to schedule the rejected event recheck after moving stray events: %w", err)
	}
	return nil
}

const repairStrayEventsSQL = `UPDATE roomserver_events AS e SET room_nid = r.room_nid
	FROM roomserver_event_json AS j, roomserver_rooms AS r
	WHERE j.event_nid = e.event_nid
	AND r.room_id = COALESCE(j.event_json::json->>'room_id', '!' || substr(e.event_id, 2))
	AND NOT EXISTS (SELECT 1 FROM roomserver_rooms AS o WHERE o.room_nid = e.room_nid);`
