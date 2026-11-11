// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlite3

import (
	"context"
	"database/sql"
	"strings"

	"codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/storage/tables"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// The events whose state was calculated from partial state, after a faster
// join (MSC3706, MSC3902). The partial-state resync recalculates the state of
// each of them from the full state, and removes it from this table in the
// same transaction, so a resync that is interrupted resumes with the events
// that are left.
const partialStateEventsSchema = `
CREATE TABLE IF NOT EXISTS roomserver_partial_state_events (
    event_nid INTEGER PRIMARY KEY,
    room_nid INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS roomserver_partial_state_events_room_idx
    ON roomserver_partial_state_events (room_nid, event_nid);
`

const insertPartialStateEventSQL = "" +
	"INSERT OR IGNORE INTO roomserver_partial_state_events (event_nid, room_nid) VALUES ($1, $2)"

const deletePartialStateEventSQL = "" +
	"DELETE FROM roomserver_partial_state_events WHERE event_nid = $1"

const deletePartialStateEventsForRoomSQL = "" +
	"DELETE FROM roomserver_partial_state_events WHERE room_nid = $1"

const selectPartialStateEventsForRoomSQL = "" +
	"SELECT event_nid FROM roomserver_partial_state_events WHERE room_nid = $1" +
	" ORDER BY event_nid ASC LIMIT $2"

const selectPartialStateEventsSQL = "" +
	"SELECT event_nid FROM roomserver_partial_state_events WHERE event_nid IN ($1)"

const selectPartialStateEventCountSQL = "" +
	"SELECT COUNT(*) FROM roomserver_partial_state_events WHERE room_nid = $1"

type partialStateEventsStatements struct {
	db                                  *sql.DB
	insertPartialStateEventStmt         *sql.Stmt
	deletePartialStateEventStmt         *sql.Stmt
	deletePartialStateEventsForRoomStmt *sql.Stmt
	selectPartialStateEventsForRoomStmt *sql.Stmt
	selectPartialStateEventCountStmt    *sql.Stmt
}

func CreatePartialStateEventsTable(db *sql.DB) error {
	_, err := db.Exec(partialStateEventsSchema)
	return err
}

func PreparePartialStateEventsTable(db *sql.DB) (tables.PartialStateEvents, error) {
	s := &partialStateEventsStatements{db: db}
	return s, sqlutil.StatementList{
		{&s.insertPartialStateEventStmt, insertPartialStateEventSQL},
		{&s.deletePartialStateEventStmt, deletePartialStateEventSQL},
		{&s.deletePartialStateEventsForRoomStmt, deletePartialStateEventsForRoomSQL},
		{&s.selectPartialStateEventsForRoomStmt, selectPartialStateEventsForRoomSQL},
		{&s.selectPartialStateEventCountStmt, selectPartialStateEventCountSQL},
	}.Prepare(db)
}

func (s *partialStateEventsStatements) InsertPartialStateEvent(
	ctx context.Context, txn *sql.Tx, roomNID types.RoomNID, eventNID types.EventNID,
) error {
	_, err := sqlutil.TxStmt(txn, s.insertPartialStateEventStmt).ExecContext(ctx, eventNID, roomNID)
	return err
}

func (s *partialStateEventsStatements) DeletePartialStateEvent(
	ctx context.Context, txn *sql.Tx, eventNID types.EventNID,
) error {
	_, err := sqlutil.TxStmt(txn, s.deletePartialStateEventStmt).ExecContext(ctx, eventNID)
	return err
}

func (s *partialStateEventsStatements) DeletePartialStateEventsForRoom(
	ctx context.Context, txn *sql.Tx, roomNID types.RoomNID,
) error {
	_, err := sqlutil.TxStmt(txn, s.deletePartialStateEventsForRoomStmt).ExecContext(ctx, roomNID)
	return err
}

func (s *partialStateEventsStatements) SelectPartialStateEventsForRoom(
	ctx context.Context, txn *sql.Tx, roomNID types.RoomNID, limit int,
) ([]types.EventNID, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectPartialStateEventsForRoomStmt).QueryContext(ctx, roomNID, limit)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectPartialStateEventsForRoom: rows.close() failed")
	return scanPartialStateEventNIDs(rows)
}

func (s *partialStateEventsStatements) SelectPartialStateEvents(
	ctx context.Context, txn *sql.Tx, eventNIDs []types.EventNID,
) ([]types.EventNID, error) {
	if len(eventNIDs) == 0 {
		return nil, nil
	}
	params := make([]any, len(eventNIDs))
	for i := range eventNIDs {
		params[i] = eventNIDs[i]
	}
	query := strings.Replace(selectPartialStateEventsSQL, "($1)", sqlutil.QueryVariadic(len(params)), 1)
	var rows *sql.Rows
	var err error
	if txn != nil {
		rows, err = txn.QueryContext(ctx, query, params...)
	} else {
		rows, err = s.db.QueryContext(ctx, query, params...)
	}
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectPartialStateEvents: rows.close() failed")
	return scanPartialStateEventNIDs(rows)
}

func (s *partialStateEventsStatements) SelectPartialStateEventCount(
	ctx context.Context, txn *sql.Tx, roomNID types.RoomNID,
) (count int64, err error) {
	err = sqlutil.TxStmt(txn, s.selectPartialStateEventCountStmt).QueryRowContext(ctx, roomNID).Scan(&count)
	return count, err
}

func scanPartialStateEventNIDs(rows *sql.Rows) ([]types.EventNID, error) {
	var nids []types.EventNID
	for rows.Next() {
		var nid types.EventNID
		if err := rows.Scan(&nid); err != nil {
			return nil, err
		}
		nids = append(nids, nid)
	}
	return nids, rows.Err()
}
