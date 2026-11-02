// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlite3

import (
	"context"
	"database/sql"
	"errors"

	"codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/storage/sqlite3/deltas"
	"codefloe.com/pat-s/zendrite/roomserver/storage/tables"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

const rejectedRecheckSchema = `
-- The progress of the one-off recheck of rejected events against the state
-- the state key repair corrected. The table has a row while it is pending.
CREATE TABLE IF NOT EXISTS roomserver_rejected_recheck (
    -- The room NID up to which rooms have been checked.
    after_room_nid BIGINT NOT NULL
);
`

const selectRejectedRecheckCursorSQL = "" +
	"SELECT after_room_nid FROM roomserver_rejected_recheck LIMIT 1"

const updateRejectedRecheckCursorSQL = "" +
	"UPDATE roomserver_rejected_recheck SET after_room_nid = $1"

const deleteRejectedRecheckSQL = "" +
	"DELETE FROM roomserver_rejected_recheck"

// Outliers, which have no state snapshot, cannot be checked again.
const selectRoomsWithRejectedEventsAfterSQL = "" +
	"SELECT DISTINCT room_nid FROM roomserver_events" +
	" WHERE room_nid > $1 AND is_rejected = 1 AND state_snapshot_nid <> 0" +
	" ORDER BY room_nid ASC LIMIT $2"

type rejectedRecheckStatements struct {
	selectRejectedRecheckCursorStmt        *sql.Stmt
	updateRejectedRecheckCursorStmt        *sql.Stmt
	deleteRejectedRecheckStmt              *sql.Stmt
	selectRoomsWithRejectedEventsAfterStmt *sql.Stmt
}

func CreateRejectedRecheckTable(db *sql.DB) error {
	if _, err := db.Exec(rejectedRecheckSchema); err != nil {
		return err
	}
	m := sqlutil.NewMigrator(db)
	m.AddMigrations(sqlutil.Migration{
		Version: "roomserver: recheck rejected events against repaired state",
		Up:      deltas.UpRejectedRecheck,
	}, sqlutil.Migration{
		Version: "roomserver: recheck rejected redactions",
		Up:      deltas.UpRejectedRecheckRedactions,
	}, sqlutil.Migration{
		Version: "roomserver: recheck rejected events after rejoins",
		Up:      deltas.UpRejectedRecheckRejoins,
	})
	return m.Up(context.Background())
}

func PrepareRejectedRecheckTable(db *sql.DB) (tables.RejectedRecheck, error) {
	s := &rejectedRecheckStatements{}
	return s, sqlutil.StatementList{
		{&s.selectRejectedRecheckCursorStmt, selectRejectedRecheckCursorSQL},
		{&s.updateRejectedRecheckCursorStmt, updateRejectedRecheckCursorSQL},
		{&s.deleteRejectedRecheckStmt, deleteRejectedRecheckSQL},
		{&s.selectRoomsWithRejectedEventsAfterStmt, selectRoomsWithRejectedEventsAfterSQL},
	}.Prepare(db)
}

func (s *rejectedRecheckStatements) SelectRejectedRecheckCursor(
	ctx context.Context, txn *sql.Tx,
) (types.RoomNID, bool, error) {
	var afterRoomNID int64
	err := sqlutil.TxStmt(txn, s.selectRejectedRecheckCursorStmt).QueryRowContext(ctx).Scan(&afterRoomNID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return types.RoomNID(afterRoomNID), true, nil
}

func (s *rejectedRecheckStatements) UpdateRejectedRecheckCursor(
	ctx context.Context, txn *sql.Tx, afterRoomNID types.RoomNID,
) error {
	_, err := sqlutil.TxStmt(txn, s.updateRejectedRecheckCursorStmt).ExecContext(ctx, afterRoomNID)
	return err
}

func (s *rejectedRecheckStatements) DeleteRejectedRecheck(ctx context.Context, txn *sql.Tx) error {
	_, err := sqlutil.TxStmt(txn, s.deleteRejectedRecheckStmt).ExecContext(ctx)
	return err
}

func (s *rejectedRecheckStatements) SelectRoomsWithRejectedEventsAfter(
	ctx context.Context, txn *sql.Tx, afterRoomNID types.RoomNID, limit int,
) ([]types.RoomNID, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectRoomsWithRejectedEventsAfterStmt).QueryContext(ctx, afterRoomNID, limit)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectRoomsWithRejectedEventsAfter: rows.close() failed")
	var result []types.RoomNID
	for rows.Next() {
		var roomNID types.RoomNID
		if err = rows.Scan(&roomNID); err != nil {
			return nil, err
		}
		result = append(result, roomNID)
	}
	return result, rows.Err()
}
