// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package postgres

import (
	"context"
	"database/sql"

	"codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/userapi/storage/tables"
)

// The device list updates of remote users that this server did not track
// when they arrived, while rooms were in partial state (MSC3902). Such a user
// may turn out to share one of those rooms, and is reported as changed once
// the room's resync is done, as Synapse does.
const partialStateDeviceListsSchema = `
CREATE TABLE IF NOT EXISTS keyserver_partial_state_device_lists (
    user_id TEXT NOT NULL,
    device_id TEXT NOT NULL,
    PRIMARY KEY (user_id, device_id)
);
`

const insertPartialStateDeviceListSQL = "" +
	"INSERT INTO keyserver_partial_state_device_lists (user_id, device_id) VALUES ($1, $2)" +
	" ON CONFLICT DO NOTHING"

const selectPartialStateDeviceListsSQL = "" +
	"SELECT user_id, device_id FROM keyserver_partial_state_device_lists"

const deletePartialStateDeviceListsSQL = "" +
	"DELETE FROM keyserver_partial_state_device_lists WHERE user_id = ANY($1)"

const deleteAllPartialStateDeviceListsSQL = "" +
	"DELETE FROM keyserver_partial_state_device_lists"

type partialStateDeviceListsStatements struct {
	insertStmt    *sql.Stmt
	selectStmt    *sql.Stmt
	deleteStmt    *sql.Stmt
	deleteAllStmt *sql.Stmt
}

func NewPostgresPartialStateDeviceListsTable(db *sql.DB) (tables.PartialStateDeviceLists, error) {
	if _, err := db.Exec(partialStateDeviceListsSchema); err != nil {
		return nil, err
	}
	s := &partialStateDeviceListsStatements{}
	return s, sqlutil.StatementList{
		{&s.insertStmt, insertPartialStateDeviceListSQL},
		{&s.selectStmt, selectPartialStateDeviceListsSQL},
		{&s.deleteStmt, deletePartialStateDeviceListsSQL},
		{&s.deleteAllStmt, deleteAllPartialStateDeviceListsSQL},
	}.Prepare(db)
}

func (s *partialStateDeviceListsStatements) InsertPartialStateDeviceList(ctx context.Context, txn *sql.Tx, userID, deviceID string) error {
	_, err := sqlutil.TxStmt(txn, s.insertStmt).ExecContext(ctx, userID, deviceID)
	return err
}

func (s *partialStateDeviceListsStatements) SelectPartialStateDeviceLists(ctx context.Context, txn *sql.Tx) (map[string][]string, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectStmt).QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectPartialStateDeviceLists: rows.close() failed")
	result := map[string][]string{}
	for rows.Next() {
		var userID, deviceID string
		if err = rows.Scan(&userID, &deviceID); err != nil {
			return nil, err
		}
		result[userID] = append(result[userID], deviceID)
	}
	return result, rows.Err()
}

func (s *partialStateDeviceListsStatements) DeletePartialStateDeviceLists(ctx context.Context, txn *sql.Tx, userIDs []string) error {
	_, err := sqlutil.TxStmt(txn, s.deleteStmt).ExecContext(ctx, userIDs)
	return err
}

func (s *partialStateDeviceListsStatements) DeleteAllPartialStateDeviceLists(ctx context.Context, txn *sql.Tx) error {
	_, err := sqlutil.TxStmt(txn, s.deleteAllStmt).ExecContext(ctx)
	return err
}
