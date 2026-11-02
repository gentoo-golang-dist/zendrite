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

const stateKeyRepairSchema = `
-- The progress of the one-off repair of state events stored under the wrong
-- state key NID. The table has a row while the repair is pending.
CREATE TABLE IF NOT EXISTS roomserver_state_key_repair (
    -- The event NID up to which events have been checked.
    after_event_nid BIGINT NOT NULL
);
`

const selectStateKeyRepairCursorSQL = "" +
	"SELECT after_event_nid FROM roomserver_state_key_repair LIMIT 1"

const updateStateKeyRepairCursorSQL = "" +
	"UPDATE roomserver_state_key_repair SET after_event_nid = $1"

const deleteStateKeyRepairSQL = "" +
	"DELETE FROM roomserver_state_key_repair"

// The event type NIDs 1 to 5 are the pre-assigned NIDs of the types an auth
// event can have: create, power levels, join rules, third party invite and
// member.
const selectAuthTypeEventsAfterSQL = "" +
	"SELECT e.event_nid, e.room_nid, e.event_type_nid, e.event_state_key_nid, j.event_json" +
	" FROM roomserver_events e JOIN roomserver_event_json j ON j.event_nid = e.event_nid" +
	" WHERE e.event_nid > $1 AND e.event_type_nid IN (1, 2, 3, 4, 5)" +
	" ORDER BY e.event_nid ASC LIMIT $2"

type stateKeyRepairStatements struct {
	selectStateKeyRepairCursorStmt *sql.Stmt
	updateStateKeyRepairCursorStmt *sql.Stmt
	deleteStateKeyRepairStmt       *sql.Stmt
	selectAuthTypeEventsAfterStmt  *sql.Stmt
}

func CreateStateKeyRepairTable(db *sql.DB) error {
	if _, err := db.Exec(stateKeyRepairSchema); err != nil {
		return err
	}
	m := sqlutil.NewMigrator(db)
	m.AddMigrations(sqlutil.Migration{
		Version: "roomserver: repair state events stored under the wrong state key",
		Up:      deltas.UpStateKeyRepair,
	})
	return m.Up(context.Background())
}

func PrepareStateKeyRepairTable(db *sql.DB) (tables.StateKeyRepair, error) {
	s := &stateKeyRepairStatements{}
	return s, sqlutil.StatementList{
		{&s.selectStateKeyRepairCursorStmt, selectStateKeyRepairCursorSQL},
		{&s.updateStateKeyRepairCursorStmt, updateStateKeyRepairCursorSQL},
		{&s.deleteStateKeyRepairStmt, deleteStateKeyRepairSQL},
		{&s.selectAuthTypeEventsAfterStmt, selectAuthTypeEventsAfterSQL},
	}.Prepare(db)
}

func (s *stateKeyRepairStatements) SelectStateKeyRepairCursor(
	ctx context.Context, txn *sql.Tx,
) (types.EventNID, bool, error) {
	var afterEventNID int64
	err := sqlutil.TxStmt(txn, s.selectStateKeyRepairCursorStmt).QueryRowContext(ctx).Scan(&afterEventNID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return types.EventNID(afterEventNID), true, nil
}

func (s *stateKeyRepairStatements) UpdateStateKeyRepairCursor(
	ctx context.Context, txn *sql.Tx, afterEventNID types.EventNID,
) error {
	_, err := sqlutil.TxStmt(txn, s.updateStateKeyRepairCursorStmt).ExecContext(ctx, afterEventNID)
	return err
}

func (s *stateKeyRepairStatements) DeleteStateKeyRepair(ctx context.Context, txn *sql.Tx) error {
	_, err := sqlutil.TxStmt(txn, s.deleteStateKeyRepairStmt).ExecContext(ctx)
	return err
}

func (s *stateKeyRepairStatements) SelectAuthTypeEventsAfter(
	ctx context.Context, txn *sql.Tx, afterEventNID types.EventNID, limit int,
) ([]tables.AuthTypeEvent, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectAuthTypeEventsAfterStmt).QueryContext(ctx, afterEventNID, limit)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectAuthTypeEventsAfter: rows.close() failed")
	var result []tables.AuthTypeEvent
	for rows.Next() {
		var ev tables.AuthTypeEvent
		if err = rows.Scan(&ev.EventNID, &ev.RoomNID, &ev.EventTypeNID, &ev.EventStateKeyNID, &ev.EventJSON); err != nil {
			return nil, err
		}
		result = append(result, ev)
	}
	return result, rows.Err()
}
