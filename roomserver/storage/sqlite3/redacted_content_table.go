// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlite3

import (
	"context"
	"database/sql"
	"errors"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/storage/tables"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

const redactedContentSchema = `
-- Keeps the JSON of redacted events as it was before the redaction, for the
-- room moderators who may view it (MSC2815), until the retention period ends.
-- roomserver_event_json holds the redacted JSON, which everything else uses.
CREATE TABLE IF NOT EXISTS roomserver_redacted_content (
    event_nid INTEGER NOT NULL PRIMARY KEY,
    room_nid INTEGER NOT NULL,
    event_json TEXT NOT NULL,
    -- When the redaction was applied, in milliseconds, for expiry.
    redacted_ts INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS roomserver_redacted_content_redacted_ts_idx ON roomserver_redacted_content (redacted_ts);
CREATE INDEX IF NOT EXISTS roomserver_redacted_content_room_nid_idx ON roomserver_redacted_content (room_nid);
`

const insertRedactedContentSQL = "" +
	"INSERT INTO roomserver_redacted_content (event_nid, room_nid, event_json, redacted_ts)" +
	" VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING"

const selectRedactedContentSQL = "" +
	"SELECT event_json FROM roomserver_redacted_content WHERE event_nid = $1 AND redacted_ts >= $2"

const deleteRedactedContentBeforeSQL = "" +
	"DELETE FROM roomserver_redacted_content WHERE redacted_ts < $1"

type redactedContentStatements struct {
	insertRedactedContentStmt       *sql.Stmt
	selectRedactedContentStmt       *sql.Stmt
	deleteRedactedContentBeforeStmt *sql.Stmt
}

func CreateRedactedContentTable(db *sql.DB) error {
	_, err := db.Exec(redactedContentSchema)
	return err
}

func PrepareRedactedContentTable(db *sql.DB) (tables.RedactedContent, error) {
	s := &redactedContentStatements{}
	return s, sqlutil.StatementList{
		{&s.insertRedactedContentStmt, insertRedactedContentSQL},
		{&s.selectRedactedContentStmt, selectRedactedContentSQL},
		{&s.deleteRedactedContentBeforeStmt, deleteRedactedContentBeforeSQL},
	}.Prepare(db)
}

func (s *redactedContentStatements) InsertRedactedContent(
	ctx context.Context, txn *sql.Tx, eventNID types.EventNID, roomNID types.RoomNID, eventJSON []byte, redactedTS int64,
) error {
	_, err := sqlutil.TxStmt(txn, s.insertRedactedContentStmt).ExecContext(
		ctx, int64(eventNID), int64(roomNID), eventJSON, redactedTS,
	)
	return err
}

func (s *redactedContentStatements) SelectRedactedContent(
	ctx context.Context, txn *sql.Tx, eventNID types.EventNID, notBeforeTS int64,
) ([]byte, error) {
	var eventJSON []byte
	err := sqlutil.TxStmt(txn, s.selectRedactedContentStmt).QueryRowContext(ctx, int64(eventNID), notBeforeTS).Scan(&eventJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return eventJSON, err
}

func (s *redactedContentStatements) DeleteRedactedContentBefore(
	ctx context.Context, txn *sql.Tx, redactedTS int64,
) (int64, error) {
	res, err := sqlutil.TxStmt(txn, s.deleteRedactedContentBeforeStmt).ExecContext(ctx, redactedTS)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
