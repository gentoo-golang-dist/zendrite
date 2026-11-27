// Copyright 2025 Jackmaninov
// Copyright 2025 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlite3

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/syncapi/storage/tables"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

const unPartialStatedRoomsSchema = `
-- Tracks rooms that have completed their partial state resync (MSC3706).
-- When a room completes its partial state resync, we insert a row for each
-- user in the room so that sync can treat the room as "newly joined".
CREATE TABLE IF NOT EXISTS syncapi_unpartialstated_rooms (
	-- The stream position ID
	id INTEGER PRIMARY KEY,
	-- The room ID that completed partial state
	room_id TEXT NOT NULL,
	-- The user ID who should see this room as "newly joined"
	user_id TEXT NOT NULL,
	-- Timestamp when the room completed partial state
	created_at INTEGER NOT NULL DEFAULT (strftime('%s','now') * 1000)
);
CREATE INDEX IF NOT EXISTS syncapi_unpartialstated_rooms_user_id_idx ON syncapi_unpartialstated_rooms(user_id);
CREATE INDEX IF NOT EXISTS syncapi_unpartialstated_rooms_room_id_idx ON syncapi_unpartialstated_rooms(room_id);

-- Rooms that a local user joined with partial state and whose un-partial-stated
-- records are not written yet (MSC3706). The roomserver says a room has its
-- full state before this component has stored it, so eager syncs leave these
-- rooms out too: otherwise a sync in between would send the room, and the
-- un-partial-stated record would send it again as newly joined.
CREATE TABLE IF NOT EXISTS syncapi_partial_state_joins (
	room_id TEXT PRIMARY KEY,
	-- A position of the PDU stream taken when the join was recorded. An
	-- un-partial-stated record of the room after it settles the join even if
	-- the row is still here, as it is when an older build, which does not know
	-- this table, wrote the records.
	pos INTEGER NOT NULL
);
`

const insertUnPartialStatedRoomSQL = "" +
	"INSERT INTO syncapi_unpartialstated_rooms (id, room_id, user_id)" +
	" VALUES ($1, $2, $3)"

const selectUnPartialStatedRoomsInRangeSQL = "" +
	"SELECT id, room_id FROM syncapi_unpartialstated_rooms" +
	" WHERE user_id = $1 AND id > $2 AND id <= $3"

const selectMaxUnPartialStatedRoomIDSQL = "" +
	"SELECT MAX(id) FROM syncapi_unpartialstated_rooms"

const purgeUnPartialStatedRoomsSQL = "" +
	"DELETE FROM syncapi_unpartialstated_rooms WHERE room_id = $1"

const insertPartialStateJoinSQL = "" +
	"INSERT INTO syncapi_partial_state_joins (room_id, pos) VALUES ($1, $2)" +
	" ON CONFLICT (room_id) DO UPDATE SET pos = excluded.pos"

const deletePartialStateJoinSQL = "" +
	"DELETE FROM syncapi_partial_state_joins WHERE room_id = $1"

// A room is still partial for a user's sync up to a position while its
// partial-state join is pending, or until the sync reaches the user's
// un-partial-stated record. A join is no longer pending once the room has an
// un-partial-stated record after it: this build deletes the join in the same
// transaction, but an older one writes the records and leaves the join behind,
// which would otherwise keep the room out of eager syncs for good.
const selectPartialStateRoomsSQL = "" +
	"SELECT j.room_id FROM syncapi_partial_state_joins j" +
	" WHERE NOT EXISTS (SELECT 1 FROM syncapi_unpartialstated_rooms u" +
	" WHERE u.room_id = j.room_id AND u.id > j.pos)" +
	" UNION SELECT room_id FROM syncapi_unpartialstated_rooms WHERE user_id = $1 AND id > $2"

// A join counts as pending here as it does for selectPartialStateRoomsSQL.
const selectPartialStateJoinSQL = "" +
	"SELECT 1 FROM syncapi_partial_state_joins j WHERE j.room_id = $1" +
	" AND NOT EXISTS (SELECT 1 FROM syncapi_unpartialstated_rooms u" +
	" WHERE u.room_id = j.room_id AND u.id > j.pos)"

type unPartialStatedRoomsStatements struct {
	db                                 *sql.DB
	streamIDStatements                 *StreamIDStatements
	insertUnPartialStatedRoomStmt      *sql.Stmt
	selectUnPartialStatedRoomsInRange  *sql.Stmt
	selectMaxUnPartialStatedRoomIDStmt *sql.Stmt
	purgeUnPartialStatedRoomsStmt      *sql.Stmt
	insertPartialStateJoinStmt         *sql.Stmt
	deletePartialStateJoinStmt         *sql.Stmt
	selectPartialStateJoinStmt         *sql.Stmt
	selectPartialStateRoomsStmt        *sql.Stmt
}

func NewSqliteUnPartialStatedRoomsTable(db *sql.DB, streamID *StreamIDStatements) (tables.UnPartialStatedRooms, error) {
	_, err := db.Exec(unPartialStatedRoomsSchema)
	if err != nil {
		return nil, err
	}
	s := &unPartialStatedRoomsStatements{
		db:                 db,
		streamIDStatements: streamID,
	}
	return s, sqlutil.StatementList{
		{&s.insertUnPartialStatedRoomStmt, insertUnPartialStatedRoomSQL},
		{&s.selectUnPartialStatedRoomsInRange, selectUnPartialStatedRoomsInRangeSQL},
		{&s.selectMaxUnPartialStatedRoomIDStmt, selectMaxUnPartialStatedRoomIDSQL},
		{&s.purgeUnPartialStatedRoomsStmt, purgeUnPartialStatedRoomsSQL},
		{&s.insertPartialStateJoinStmt, insertPartialStateJoinSQL},
		{&s.deletePartialStateJoinStmt, deletePartialStateJoinSQL},
		{&s.selectPartialStateJoinStmt, selectPartialStateJoinSQL},
		{&s.selectPartialStateRoomsStmt, selectPartialStateRoomsSQL},
	}.Prepare(db)
}

func (s *unPartialStatedRoomsStatements) InsertUnPartialStatedRoom(
	ctx context.Context, txn *sql.Tx, roomID, userID string,
) (pos types.StreamPosition, err error) {
	// The position is one of the PDU stream, which the sync compares it with.
	pos, err = s.streamIDStatements.nextPDUID(ctx, txn)
	if err != nil {
		return
	}
	stmt := sqlutil.TxStmt(txn, s.insertUnPartialStatedRoomStmt)
	_, err = stmt.ExecContext(ctx, pos, roomID, userID)
	return
}

func (s *unPartialStatedRoomsStatements) SelectUnPartialStatedRoomsInRange(
	ctx context.Context, txn *sql.Tx, userID string, r types.Range,
) ([]string, types.StreamPosition, error) {
	var lastPos types.StreamPosition
	selectUnPartialStatedRoomsInRange := sqlutil.TxStmt(txn, s.selectUnPartialStatedRoomsInRange)
	rows, err := selectUnPartialStatedRoomsInRange.QueryContext(ctx, userID, r.Low(), r.High())
	if err != nil {
		return nil, 0, fmt.Errorf("unable to query un-partial-stated rooms: %w", err)
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectUnPartialStatedRoomsInRange: rows.close() failed")

	var roomIDs []string
	for rows.Next() {
		var id types.StreamPosition
		var roomID string
		if err = rows.Scan(&id, &roomID); err != nil {
			return nil, 0, fmt.Errorf("unable to scan row: %w", err)
		}
		roomIDs = append(roomIDs, roomID)
		if id > lastPos {
			lastPos = id
		}
	}
	return roomIDs, lastPos, rows.Err()
}

func (s *unPartialStatedRoomsStatements) SelectMaxUnPartialStatedRoomID(
	ctx context.Context, txn *sql.Tx,
) (id int64, err error) {
	var nullableID sql.NullInt64
	stmt := sqlutil.TxStmt(txn, s.selectMaxUnPartialStatedRoomIDStmt)
	err = stmt.QueryRowContext(ctx).Scan(&nullableID)
	if nullableID.Valid {
		id = nullableID.Int64
	}
	return
}

func (s *unPartialStatedRoomsStatements) PurgeUnPartialStatedRooms(
	ctx context.Context, txn *sql.Tx, roomID string,
) error {
	purgeUnPartialStatedRoomsStmt := sqlutil.TxStmt(txn, s.purgeUnPartialStatedRoomsStmt)
	if _, err := purgeUnPartialStatedRoomsStmt.ExecContext(ctx, roomID); err != nil {
		return err
	}
	return s.DeletePartialStateJoin(ctx, txn, roomID)
}

func (s *unPartialStatedRoomsStatements) InsertPartialStateJoin(
	ctx context.Context, txn *sql.Tx, roomID string,
) error {
	// The position is one of the PDU stream, as are those of the records.
	pos, err := s.streamIDStatements.nextPDUID(ctx, txn)
	if err != nil {
		return err
	}
	_, err = sqlutil.TxStmt(txn, s.insertPartialStateJoinStmt).ExecContext(ctx, roomID, pos)
	return err
}

func (s *unPartialStatedRoomsStatements) SelectPartialStateJoin(
	ctx context.Context, txn *sql.Tx, roomID string,
) (bool, error) {
	var one int
	err := sqlutil.TxStmt(txn, s.selectPartialStateJoinStmt).QueryRowContext(ctx, roomID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *unPartialStatedRoomsStatements) DeletePartialStateJoin(
	ctx context.Context, txn *sql.Tx, roomID string,
) error {
	_, err := sqlutil.TxStmt(txn, s.deletePartialStateJoinStmt).ExecContext(ctx, roomID)
	return err
}

func (s *unPartialStatedRoomsStatements) SelectPartialStateRooms(
	ctx context.Context, txn *sql.Tx, userID string, to types.StreamPosition,
) ([]string, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectPartialStateRoomsStmt).QueryContext(ctx, userID, to)
	if err != nil {
		return nil, fmt.Errorf("unable to query partial state rooms: %w", err)
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectPartialStateRooms: rows.close() failed")
	var roomIDs []string
	for rows.Next() {
		var roomID string
		if err = rows.Scan(&roomID); err != nil {
			return nil, fmt.Errorf("unable to scan row: %w", err)
		}
		roomIDs = append(roomIDs, roomID)
	}
	return roomIDs, rows.Err()
}
