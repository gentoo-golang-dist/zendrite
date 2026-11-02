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
	"codefloe.com/pat-s/zendrite/syncapi/storage/tables"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

const profileUpdatesSchema = `
CREATE SEQUENCE IF NOT EXISTS syncapi_profile_update_id;

-- Records which profile field of which local user changed, in a stream that
-- the MSC4262 sliding sync extension reads from. The values themselves stay
-- in the user API's profile tables.
CREATE TABLE IF NOT EXISTS syncapi_profile_updates (
	-- The position in the profile updates stream
	id BIGINT PRIMARY KEY DEFAULT nextval('syncapi_profile_update_id'),
	-- The user whose profile changed
	user_id TEXT NOT NULL,
	-- The name of the field that was set or removed
	field_name TEXT NOT NULL
);
`

const insertProfileUpdateSQL = "" +
	"INSERT INTO syncapi_profile_updates (user_id, field_name) VALUES ($1, $2) RETURNING id"

const selectProfileUpdatesInRangeSQL = "" +
	"SELECT id, user_id, field_name FROM syncapi_profile_updates" +
	" WHERE id > $1 AND id <= $2 ORDER BY id ASC LIMIT $3"

const selectMaxProfileUpdateIDSQL = "" +
	"SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM syncapi_profile_update_id"

type profileUpdatesStatements struct {
	insertProfileUpdateStmt         *sql.Stmt
	selectProfileUpdatesInRangeStmt *sql.Stmt
	selectMaxProfileUpdateIDStmt    *sql.Stmt
}

func NewPostgresProfileUpdatesTable(db *sql.DB) (tables.ProfileUpdates, error) {
	if _, err := db.Exec(profileUpdatesSchema); err != nil {
		return nil, err
	}
	s := &profileUpdatesStatements{}
	return s, sqlutil.StatementList{
		{&s.insertProfileUpdateStmt, insertProfileUpdateSQL},
		{&s.selectProfileUpdatesInRangeStmt, selectProfileUpdatesInRangeSQL},
		{&s.selectMaxProfileUpdateIDStmt, selectMaxProfileUpdateIDSQL},
	}.Prepare(db)
}

func (s *profileUpdatesStatements) InsertProfileUpdate(
	ctx context.Context, txn *sql.Tx, userID, fieldName string,
) (pos types.StreamPosition, err error) {
	err = sqlutil.TxStmt(txn, s.insertProfileUpdateStmt).QueryRowContext(ctx, userID, fieldName).Scan(&pos)
	return pos, err
}

func (s *profileUpdatesStatements) SelectProfileUpdatesInRange(
	ctx context.Context, txn *sql.Tx, from, to types.StreamPosition, limit int,
) ([]types.ProfileUpdate, error) {
	if to <= from || limit <= 0 {
		return nil, nil
	}
	rows, err := sqlutil.TxStmt(txn, s.selectProfileUpdatesInRangeStmt).QueryContext(ctx, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectProfileUpdatesInRange: rows.close() failed")
	return scanProfileUpdates(rows)
}

func (s *profileUpdatesStatements) SelectMaxProfileUpdateID(
	ctx context.Context, txn *sql.Tx,
) (id int64, err error) {
	err = sqlutil.TxStmt(txn, s.selectMaxProfileUpdateIDStmt).QueryRowContext(ctx).Scan(&id)
	return id, err
}

func scanProfileUpdates(rows *sql.Rows) ([]types.ProfileUpdate, error) {
	var updates []types.ProfileUpdate
	for rows.Next() {
		var update types.ProfileUpdate
		if err := rows.Scan(&update.Position, &update.UserID, &update.Field); err != nil {
			return nil, err
		}
		updates = append(updates, update)
	}
	return updates, rows.Err()
}
