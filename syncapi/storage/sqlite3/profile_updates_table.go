// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlite3

import (
	"context"
	"database/sql"

	"codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/syncapi/storage/tables"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

const profileUpdatesSchema = `
-- Records which profile field of which local user changed, in a stream that
-- the MSC4262 sliding sync extension reads from. The values themselves stay
-- in the user API's profile tables.
CREATE TABLE IF NOT EXISTS syncapi_profile_updates (
	-- The position in the profile updates stream
	id INTEGER PRIMARY KEY,
	-- The user whose profile changed
	user_id TEXT NOT NULL,
	-- The name of the field that was set or removed
	field_name TEXT NOT NULL
);
`

const insertProfileUpdateSQL = "" +
	"INSERT INTO syncapi_profile_updates (id, user_id, field_name) VALUES ($1, $2, $3)"

const selectProfileUpdatesInRangeSQL = "" +
	"SELECT id, user_id, field_name FROM syncapi_profile_updates" +
	" WHERE id > $1 AND id <= $2 ORDER BY id ASC LIMIT $3"

const selectMaxProfileUpdateIDSQL = "" +
	"SELECT stream_id FROM syncapi_stream_id WHERE stream_name = 'profile'"

type profileUpdatesStatements struct {
	streamIDStatements              *StreamIDStatements
	insertProfileUpdateStmt         *sql.Stmt
	selectProfileUpdatesInRangeStmt *sql.Stmt
	selectMaxProfileUpdateIDStmt    *sql.Stmt
}

func NewSqliteProfileUpdatesTable(db *sql.DB, streamID *StreamIDStatements) (tables.ProfileUpdates, error) {
	if _, err := db.Exec(profileUpdatesSchema); err != nil {
		return nil, err
	}
	s := &profileUpdatesStatements{streamIDStatements: streamID}
	return s, sqlutil.StatementList{
		{&s.insertProfileUpdateStmt, insertProfileUpdateSQL},
		{&s.selectProfileUpdatesInRangeStmt, selectProfileUpdatesInRangeSQL},
		{&s.selectMaxProfileUpdateIDStmt, selectMaxProfileUpdateIDSQL},
	}.Prepare(db)
}

func (s *profileUpdatesStatements) InsertProfileUpdate(
	ctx context.Context, txn *sql.Tx, userID, fieldName string,
) (types.StreamPosition, error) {
	pos, err := s.streamIDStatements.nextProfileID(ctx, txn)
	if err != nil {
		return 0, err
	}
	_, err = sqlutil.TxStmt(txn, s.insertProfileUpdateStmt).ExecContext(ctx, pos, userID, fieldName)
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
	var updates []types.ProfileUpdate
	for rows.Next() {
		var update types.ProfileUpdate
		if err = rows.Scan(&update.Position, &update.UserID, &update.Field); err != nil {
			return nil, err
		}
		updates = append(updates, update)
	}
	return updates, rows.Err()
}

func (s *profileUpdatesStatements) SelectMaxProfileUpdateID(
	ctx context.Context, txn *sql.Tx,
) (id int64, err error) {
	err = sqlutil.TxStmt(txn, s.selectMaxProfileUpdateIDStmt).QueryRowContext(ctx).Scan(&id)
	return id, err
}
