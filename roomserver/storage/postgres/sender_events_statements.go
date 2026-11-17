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
	"codefloe.com/pat-s/zendrite/roomserver/storage/tables"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// selectSenderEventCandidatesSQL finds events of a room before an event NID,
// newest first through roomserver_events_room_nid_idx, whose JSON contains a
// sender's ID and that no validated redaction covers (MSC4194). The event
// table has no sender column, and the JSON is not parsed here: PostgreSQL's
// JSON functions reject some strings that event JSON may contain. The caller
// checks the sender of each candidate.
const selectSenderEventCandidatesSQL = "" +
	"SELECT e.event_nid, e.event_id, e.sent_to_output, e.state_snapshot_nid, j.event_json FROM roomserver_events e" +
	" JOIN roomserver_event_json j ON j.event_nid = e.event_nid" +
	" WHERE e.room_nid = $1 AND strpos(j.event_json, $2) > 0 AND e.event_nid < $3" +
	" AND e.is_rejected = FALSE AND e.event_type_nid NOT IN ($4, $5)" +
	" AND NOT EXISTS (SELECT 1 FROM roomserver_redactions r WHERE r.redacts_event_id = e.event_id AND r.validated = TRUE)" +
	" ORDER BY e.event_nid DESC LIMIT $6"

type senderEventStatements struct {
	selectSenderEventCandidatesStmt *sql.Stmt
}

// PrepareSenderEventStatements prepares the search for a sender's events
// (MSC4194), which reads the events, event JSON and redactions tables.
func PrepareSenderEventStatements(db *sql.DB) (tables.SenderEvents, error) {
	s := &senderEventStatements{}
	return s, sqlutil.StatementList{
		{&s.selectSenderEventCandidatesStmt, selectSenderEventCandidatesSQL},
	}.Prepare(db)
}

func (s *senderEventStatements) SelectSenderEventCandidates(
	ctx context.Context, txn *sql.Tx, roomNID types.RoomNID, needle string, beforeNID types.EventNID, limit int,
) ([]tables.SenderEventCandidate, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectSenderEventCandidatesStmt).QueryContext(
		ctx, roomNID, needle, beforeNID, types.MRoomCreateNID, types.MRoomRedactionNID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectSenderEventCandidates: rows.close() failed")
	var result []tables.SenderEventCandidate
	for rows.Next() {
		var (
			c             tables.SenderEventCandidate
			sentToOutput  bool
			stateSnapshot types.StateSnapshotNID
		)
		if err = rows.Scan(&c.EventNID, &c.EventID, &sentToOutput, &stateSnapshot, &c.EventJSON); err != nil {
			return nil, err
		}
		// Soft failure is not recorded. A soft-failed event has state but
		// was never delivered as a new event, which is also true of history
		// fetched by backfill.
		c.Undelivered = !sentToOutput && stateSnapshot != 0
		result = append(result, c)
	}
	return result, rows.Err()
}
