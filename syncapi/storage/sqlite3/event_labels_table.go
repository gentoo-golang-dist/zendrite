// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlite3

import (
	"context"
	"database/sql"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/syncapi/storage/tables"
)

const eventLabelsSchema = `
-- The labels of stored events (MSC2326), from the org.matrix.labels field of
-- their content, so that room event filters can select events by label. Rows
-- are removed when the event is redacted.
CREATE TABLE IF NOT EXISTS syncapi_event_labels (
	event_id TEXT NOT NULL,
	room_id TEXT NOT NULL,
	label TEXT NOT NULL,
	UNIQUE (event_id, label)
);
CREATE INDEX IF NOT EXISTS syncapi_event_labels_room_id_idx ON syncapi_event_labels(room_id, label);
`

const insertEventLabelSQL = "" +
	"INSERT INTO syncapi_event_labels (event_id, room_id, label) VALUES ($1, $2, $3)" +
	" ON CONFLICT (event_id, label) DO NOTHING"

const deleteEventLabelsSQL = "" +
	"DELETE FROM syncapi_event_labels WHERE event_id = $1"

const purgeEventLabelsSQL = "" +
	"DELETE FROM syncapi_event_labels WHERE room_id = $1"

type eventLabelsStatements struct {
	insertEventLabelStmt  *sql.Stmt
	deleteEventLabelsStmt *sql.Stmt
	purgeEventLabelsStmt  *sql.Stmt
}

// NewSqliteEventLabelsTable creates the event labels table, and fills it
// from the events stored before it existed. It needs the output room events
// table to exist.
func NewSqliteEventLabelsTable(db *sql.DB) (tables.EventLabels, error) {
	if _, err := db.Exec(eventLabelsSchema); err != nil {
		return nil, err
	}
	s := &eventLabelsStatements{}
	return s, sqlutil.StatementList{
		{&s.insertEventLabelStmt, insertEventLabelSQL},
		{&s.deleteEventLabelsStmt, deleteEventLabelsSQL},
		{&s.purgeEventLabelsStmt, purgeEventLabelsSQL},
	}.Prepare(db)
}

func (s *eventLabelsStatements) InsertEventLabels(
	ctx context.Context, txn *sql.Tx, roomID, eventID string, labels []string,
) error {
	stmt := sqlutil.TxStmt(txn, s.insertEventLabelStmt)
	for _, label := range labels {
		if _, err := stmt.ExecContext(ctx, eventID, roomID, label); err != nil {
			return err
		}
	}
	return nil
}

func (s *eventLabelsStatements) DeleteEventLabels(ctx context.Context, txn *sql.Tx, eventID string) error {
	_, err := sqlutil.TxStmt(txn, s.deleteEventLabelsStmt).ExecContext(ctx, eventID)
	return err
}

func (s *eventLabelsStatements) PurgeEventLabels(ctx context.Context, txn *sql.Tx, roomID string) error {
	_, err := sqlutil.TxStmt(txn, s.purgeEventLabelsStmt).ExecContext(ctx, roomID)
	return err
}
