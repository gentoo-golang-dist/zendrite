// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlite3

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/syncapi/storage/sqlite3/deltas"
	"codefloe.com/pat-s/zendrite/syncapi/storage/tables"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

const receiptsSchema = `
-- Stores data about receipts
CREATE TABLE IF NOT EXISTS syncapi_receipts (
	-- The ID
	id BIGINT,
	room_id TEXT NOT NULL,
	receipt_type TEXT NOT NULL,
	user_id TEXT NOT NULL,
	event_id TEXT NOT NULL,
	receipt_ts BIGINT NOT NULL,
	CONSTRAINT syncapi_receipts_unique UNIQUE (room_id, receipt_type, user_id)
);
CREATE INDEX IF NOT EXISTS syncapi_receipts_room_id_idx ON syncapi_receipts(room_id);
`

const upsertReceipt = "" +
	"INSERT INTO syncapi_receipts" +
	" (id, room_id, receipt_type, user_id, event_id, thread_id, receipt_ts)" +
	" VALUES ($1, $2, $3, $4, $5, $6, $7)" +
	" ON CONFLICT (room_id, receipt_type, user_id, thread_id)" +
	" DO UPDATE SET id = CASE" +
	"   WHEN syncapi_receipts.event_id != excluded.event_id THEN excluded.id" +
	"   ELSE syncapi_receipts.id" +
	" END, event_id = excluded.event_id, receipt_ts = excluded.receipt_ts"

const selectRoomReceipts = "" +
	"SELECT id, room_id, receipt_type, user_id, event_id, thread_id, receipt_ts" +
	" FROM syncapi_receipts" +
	" WHERE id > $1 and room_id in ($2)"

const selectMaxReceiptIDSQL = "" +
	"SELECT MAX(id) FROM syncapi_receipts"

const purgeReceiptsSQL = "" +
	"DELETE FROM syncapi_receipts WHERE room_id = $1"

const selectConnectionReceiptsSQL = "" +
	"SELECT room_id, receipt_type, user_id, last_delivered_event_id, thread_id " +
	"FROM syncapi_sliding_sync_connection_receipts " +
	"WHERE connection_key = $1"

const upsertConnectionReceiptSQL = "" +
	"INSERT INTO syncapi_sliding_sync_connection_receipts " +
	"(connection_key, room_id, receipt_type, user_id, last_delivered_event_id, thread_id, last_delivered_ts, connection_position) " +
	"VALUES ($1, $2, $3, $4, $5, $6, $7, $8) " +
	"ON CONFLICT (connection_key, room_id, receipt_type, user_id, thread_id) " +
	"DO UPDATE SET last_delivered_event_id = $5, last_delivered_ts = $7, connection_position = $8"

const deleteConnectionReceiptsSQL = "" +
	"DELETE FROM syncapi_sliding_sync_connection_receipts WHERE connection_key = $1"

const deleteConnectionReceiptsForRoomSQL = "" +
	"DELETE FROM syncapi_sliding_sync_connection_receipts WHERE connection_key = $1 AND room_id = $2"

// Receipts recorded for a response stay pending, under the response's
// position, until a request carries that position: they are then
// acknowledged (position 0), and the pending receipts of every other
// response, which the client never saw, are forgotten (MSC4551).
const acknowledgeConnectionReceiptsSQL = "" +
	"UPDATE syncapi_sliding_sync_connection_receipts SET connection_position = 0 WHERE connection_key = $1 AND connection_position = $2"

const forgetPendingConnectionReceiptsSQL = "" +
	"DELETE FROM syncapi_sliding_sync_connection_receipts WHERE connection_key = $1 AND connection_position <> 0"

type receiptStatements struct {
	db                              *sql.DB
	streamIDStatements              *StreamIDStatements
	upsertReceipt                   *sql.Stmt
	selectRoomReceipts              *sql.Stmt
	selectMaxReceiptID              *sql.Stmt
	purgeReceiptsStmt               *sql.Stmt
	selectConnectionReceipts        *sql.Stmt
	upsertConnectionReceipt         *sql.Stmt
	deleteConnectionReceipts        *sql.Stmt
	deleteConnectionReceiptsForRoom *sql.Stmt
	acknowledgeConnectionReceipts   *sql.Stmt
	forgetPendingConnectionReceipts *sql.Stmt
}

func NewSqliteReceiptsTable(db *sql.DB, streamID *StreamIDStatements) (tables.Receipts, error) {
	_, err := db.Exec(receiptsSchema)
	if err != nil {
		return nil, err
	}
	m := sqlutil.NewMigrator(db)
	m.AddMigrations(
		sqlutil.Migration{
			Version: "syncapi: fix sequences",
			Up:      deltas.UpFixSequences,
		},
		sqlutil.Migration{
			Version: "syncapi: create sliding sync tables",
			Up:      deltas.UpCreateSlidingSyncTables,
		},
		sqlutil.Migration{
			Version: "syncapi: add connection receipts table for sliding sync",
			Up:      deltas.UpAddConnectionReceipts,
		},
		sqlutil.Migration{Version: "syncapi: threaded receipts", Up: deltas.UpThreadedReceipts},
		sqlutil.Migration{Version: "syncapi: connection receipt positions", Up: deltas.UpConnectionReceiptPositions},
	)
	err = m.Up(context.Background())
	if err != nil {
		return nil, err
	}
	r := &receiptStatements{
		db:                 db,
		streamIDStatements: streamID,
	}
	return r, sqlutil.StatementList{
		{&r.upsertReceipt, upsertReceipt},
		{&r.selectRoomReceipts, selectRoomReceipts},
		{&r.selectMaxReceiptID, selectMaxReceiptIDSQL},
		{&r.purgeReceiptsStmt, purgeReceiptsSQL},
		{&r.selectConnectionReceipts, selectConnectionReceiptsSQL},
		{&r.upsertConnectionReceipt, upsertConnectionReceiptSQL},
		{&r.deleteConnectionReceipts, deleteConnectionReceiptsSQL},
		{&r.deleteConnectionReceiptsForRoom, deleteConnectionReceiptsForRoomSQL},
		{&r.acknowledgeConnectionReceipts, acknowledgeConnectionReceiptsSQL},
		{&r.forgetPendingConnectionReceipts, forgetPendingConnectionReceiptsSQL},
	}.Prepare(db)
}

// UpsertReceipt creates new user receipts.
func (r *receiptStatements) UpsertReceipt(ctx context.Context, txn *sql.Tx, roomId, receiptType, userId, eventId, threadID string, timestamp spec.Timestamp) (pos types.StreamPosition, err error) {
	// Always generate a new ID - the CASE expression in SQL will decide whether to use it
	pos, err = r.streamIDStatements.nextReceiptID(ctx, txn)
	if err != nil {
		return
	}
	stmt := sqlutil.TxStmt(txn, r.upsertReceipt)
	_, err = stmt.ExecContext(ctx, pos, roomId, receiptType, userId, eventId, threadID, timestamp)
	return
}

// SelectRoomReceiptsAfter select all receipts for a given room after a specific timestamp.
func (r *receiptStatements) SelectRoomReceiptsAfter(ctx context.Context, txn *sql.Tx, roomIDs []string, streamPos types.StreamPosition) (types.StreamPosition, []types.OutputReceiptEvent, error) {
	if len(roomIDs) == 0 {
		return streamPos, nil, nil
	}
	selectSQL := strings.Replace(selectRoomReceipts, "($2)", sqlutil.QueryVariadicOffset(len(roomIDs), 1), 1)
	var lastPos types.StreamPosition
	params := make([]any, len(roomIDs)+1)
	params[0] = streamPos
	for k, v := range roomIDs {
		params[k+1] = v
	}
	var prep *sql.Stmt
	var err error
	if txn != nil {
		prep, err = txn.PrepareContext(ctx, selectSQL)
	} else {
		prep, err = r.db.PrepareContext(ctx, selectSQL)
	}
	if err != nil {
		return 0, nil, fmt.Errorf("unable to prepare statement: %w", err)
	}
	defer internal.CloseAndLogIfError(ctx, prep, "SelectRoomReceiptsAfter: prep.close() failed")
	rows, err := prep.QueryContext(ctx, params...)
	if err != nil {
		return 0, nil, fmt.Errorf("unable to query room receipts: %w", err)
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectRoomReceiptsAfter: rows.close() failed")
	var res []types.OutputReceiptEvent
	for rows.Next() {
		r := types.OutputReceiptEvent{}
		var id types.StreamPosition
		err = rows.Scan(&id, &r.RoomID, &r.Type, &r.UserID, &r.EventID, &r.ThreadID, &r.Timestamp)
		if err != nil {
			return 0, res, fmt.Errorf("unable to scan row to api.Receipts: %w", err)
		}
		res = append(res, r)
		if id > lastPos {
			lastPos = id
		}
	}
	return lastPos, res, rows.Err()
}

func (s *receiptStatements) SelectMaxReceiptID(
	ctx context.Context, txn *sql.Tx,
) (id int64, err error) {
	var nullableID sql.NullInt64
	stmt := sqlutil.TxStmt(txn, s.selectMaxReceiptID)
	err = stmt.QueryRowContext(ctx).Scan(&nullableID)
	if nullableID.Valid {
		id = nullableID.Int64
	}
	return
}

func (s *receiptStatements) PurgeReceipts(
	ctx context.Context, txn *sql.Tx, roomID string,
) error {
	purgeReceiptsStmt := sqlutil.TxStmt(txn, s.purgeReceiptsStmt)
	_, err := purgeReceiptsStmt.ExecContext(ctx, roomID)
	return err
}

// SelectConnectionReceiptState returns the current receipts of the rooms,
// what the connection was last given of each receipt, and which rooms have
// been in the receipts extension's scope on it (MSC4551). A room in scope
// has a row with an empty receipt type.
//
// Private receipts are filtered in v4_extensions.go, not here.
func (s *receiptStatements) SelectConnectionReceiptState(
	ctx context.Context,
	txn *sql.Tx,
	connectionKey int64,
	roomIDs []string,
) (*types.ConnectionReceiptState, error) {
	state := &types.ConnectionReceiptState{
		Delivered: make(map[types.ReceiptKey]string),
		InScope:   make(map[string]bool),
	}
	if len(roomIDs) == 0 {
		return state, nil
	}
	// syncapi_receipts holds one receipt per user, receipt type and thread.
	_, latest, err := s.SelectRoomReceiptsAfter(ctx, txn, roomIDs, 0)
	if err != nil {
		return nil, err
	}
	state.Latest = latest

	selectConnectionReceipts := sqlutil.TxStmt(txn, s.selectConnectionReceipts)
	deliveredRows, err := selectConnectionReceipts.QueryContext(ctx, connectionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to query connection receipts: %w", err)
	}
	defer internal.CloseAndLogIfError(ctx, deliveredRows, "SelectConnectionReceiptState: deliveredRows.close() failed")
	for deliveredRows.Next() {
		var key types.ReceiptKey
		var eventID string
		if err = deliveredRows.Scan(&key.RoomID, &key.Type, &key.UserID, &eventID, &key.ThreadID); err != nil {
			return nil, fmt.Errorf("failed to scan connection receipt: %w", err)
		}
		if key.Type == "" {
			state.InScope[key.RoomID] = true
		} else {
			state.Delivered[key] = eventID
		}
	}
	if err = deliveredRows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate connection receipts: %w", err)
	}
	return state, nil
}

// UpsertConnectionReceipt records the receipt that the response at a
// connection position delivered. An empty receipt type marks the room as in
// the receipts extension's scope.
func (s *receiptStatements) UpsertConnectionReceipt(
	ctx context.Context,
	txn *sql.Tx,
	connectionKey, connectionPosition int64,
	roomID, receiptType, userID, eventID, threadID string,
	timestamp spec.Timestamp,
) error {
	upsertConnectionReceipt := sqlutil.TxStmt(txn, s.upsertConnectionReceipt)
	_, err := upsertConnectionReceipt.ExecContext(
		ctx, connectionKey, roomID, receiptType, userID, eventID, threadID, timestamp, connectionPosition,
	)
	return err
}

// AcknowledgeConnectionReceipts acknowledges what the response at a
// connection position delivered of receipts, and forgets what every other
// response delivered: the client did not see them, so their receipts are
// sent again (MSC4551).
func (s *receiptStatements) AcknowledgeConnectionReceipts(
	ctx context.Context,
	txn *sql.Tx,
	connectionKey, connectionPosition int64,
) error {
	if _, err := sqlutil.TxStmt(txn, s.acknowledgeConnectionReceipts).ExecContext(ctx, connectionKey, connectionPosition); err != nil {
		return err
	}
	_, err := sqlutil.TxStmt(txn, s.forgetPendingConnectionReceipts).ExecContext(ctx, connectionKey)
	return err
}

// DeleteConnectionReceipts removes all delivered receipt state for a connection.
// This should be called on fresh sync (no pos token) to ensure receipts are re-delivered.
func (s *receiptStatements) DeleteConnectionReceipts(
	ctx context.Context,
	txn *sql.Tx,
	connectionKey int64,
) error {
	deleteConnectionReceipts := sqlutil.TxStmt(txn, s.deleteConnectionReceipts)
	_, err := deleteConnectionReceipts.ExecContext(ctx, connectionKey)
	return err
}

// DeleteConnectionReceiptsForRoom removes delivered receipt state for a specific room on a connection,
// which then enters the receipts extension's scope for the first time again (MSC4551).
func (s *receiptStatements) DeleteConnectionReceiptsForRoom(
	ctx context.Context,
	txn *sql.Tx,
	connectionKey int64,
	roomID string,
) error {
	deleteConnectionReceiptsForRoom := sqlutil.TxStmt(txn, s.deleteConnectionReceiptsForRoom)
	_, err := deleteConnectionReceiptsForRoom.ExecContext(ctx, connectionKey, roomID)
	return err
}
