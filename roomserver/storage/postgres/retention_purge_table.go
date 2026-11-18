// Copyright 2026 The Zendrite Authors
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

// selectExpiredEventCandidatesSQL lists the non-state events of a room, in
// event NID order after a given NID, that may have been sent before a given
// origin_server_ts (MSC1763). The caller checks the timestamp again.
const selectExpiredEventCandidatesSQL = `
SELECT e.event_nid, e.event_id, j.event_json
FROM roomserver_events e JOIN roomserver_event_json j ON j.event_nid = e.event_nid
WHERE e.room_nid = $1 AND e.event_state_key_nid = 0 AND e.event_nid > $2
AND CASE
	WHEN j.event_json LIKE '%\\u%' THEN TRUE
	ELSE (j.event_json::json ->> 'origin_server_ts')::NUMERIC < $3
END
ORDER BY e.event_nid ASC LIMIT $4`

// The rows of a purged event. State events are never purged, so the event
// is not in any state snapshot, membership or invite. The previous events
// rows stay: they record that the event was referenced, so that it does not
// become a forward extremity again if it is received again.
const (
	purgeExpiredEventJSONSQL         = "DELETE FROM roomserver_event_json WHERE event_nid = $1"
	purgeExpiredEventSQL             = "DELETE FROM roomserver_events WHERE event_nid = $1"
	purgeExpiredPreviousEventSQL     = "DELETE FROM roomserver_previous_events WHERE previous_event_id = $1"
	purgeExpiredRedactionsSQL        = "DELETE FROM roomserver_redactions WHERE redaction_event_id = $1 OR redacts_event_id = $1"
	purgeExpiredRedactedContentSQL   = "DELETE FROM roomserver_redacted_content WHERE event_nid = $1"
	purgeExpiredAnnotationsSQL       = "DELETE FROM roomserver_annotations WHERE room_nid = $1 AND (annotation_event_id = $2 OR target_event_id = $2)"
	purgeExpiredClientTransactionSQL = "DELETE FROM roomserver_client_transactions WHERE room_id = $1 AND event_id = $2"
)

type retentionPurgeStatements struct {
	selectExpiredEventCandidatesStmt *sql.Stmt
	purgeEventJSONStmt               *sql.Stmt
	purgeEventStmt                   *sql.Stmt
	purgeRedactionsStmt              *sql.Stmt
	purgeRedactedContentStmt         *sql.Stmt
	purgeAnnotationsStmt             *sql.Stmt
	purgeClientTransactionStmt       *sql.Stmt
}

func PrepareRetentionPurgeStatements(db *sql.DB) (tables.RetentionPurge, error) {
	s := &retentionPurgeStatements{}
	return s, sqlutil.StatementList{
		{&s.selectExpiredEventCandidatesStmt, selectExpiredEventCandidatesSQL},
		{&s.purgeEventJSONStmt, purgeExpiredEventJSONSQL},
		{&s.purgeEventStmt, purgeExpiredEventSQL},
		{&s.purgeRedactionsStmt, purgeExpiredRedactionsSQL},
		{&s.purgeRedactedContentStmt, purgeExpiredRedactedContentSQL},
		{&s.purgeAnnotationsStmt, purgeExpiredAnnotationsSQL},
		{&s.purgeClientTransactionStmt, purgeExpiredClientTransactionSQL},
	}.Prepare(db)
}

func (s *retentionPurgeStatements) SelectExpiredEventCandidates(
	ctx context.Context, txn *sql.Tx, roomNID types.RoomNID, afterNID types.EventNID, beforeTS int64, limit int,
) ([]tables.ExpiredEventCandidate, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectExpiredEventCandidatesStmt).QueryContext(ctx, roomNID, afterNID, beforeTS, limit)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectExpiredEventCandidates: rows.close() failed")
	var candidates []tables.ExpiredEventCandidate
	for rows.Next() {
		var c tables.ExpiredEventCandidate
		if err = rows.Scan(&c.EventNID, &c.EventID, &c.EventJSON); err != nil {
			return nil, err
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

func (s *retentionPurgeStatements) PurgeEvent(
	ctx context.Context, txn *sql.Tx, roomNID types.RoomNID, roomID string, eventNID types.EventNID, eventID string,
) error {
	for _, step := range []struct {
		stmt *sql.Stmt
		args []any
	}{
		{s.purgeRedactionsStmt, []any{eventID}},
		{s.purgeRedactedContentStmt, []any{eventNID}},
		{s.purgeAnnotationsStmt, []any{roomNID, eventID}},
		{s.purgeClientTransactionStmt, []any{roomID, eventID}},
		{s.purgeEventJSONStmt, []any{eventNID}},
		{s.purgeEventStmt, []any{eventNID}},
	} {
		if _, err := sqlutil.TxStmt(txn, step.stmt).ExecContext(ctx, step.args...); err != nil {
			return err
		}
	}
	return nil
}
