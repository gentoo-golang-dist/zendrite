// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package postgres

import (
	"context"
	"database/sql"
	"errors"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/userapi/api"
	"codefloe.com/pat-s/zendrite/userapi/storage/tables"
)

const rendezvousSchema = `
-- Rendezvous sessions of QR code login (MSC4108 over MSC4388). They live
-- for a few minutes; the payload is at most a few kilobytes.
CREATE TABLE IF NOT EXISTS userapi_rendezvous_sessions (
	session_id TEXT NOT NULL PRIMARY KEY,
	-- The API that created the session: msc4388 or msc4108.
	kind TEXT NOT NULL,
	data BYTEA NOT NULL,
	-- The media type of an MSC4108 payload, empty for MSC4388.
	content_type TEXT NOT NULL,
	-- Grows by one with every successful write, starting at 1.
	sequence BIGINT NOT NULL,
	last_modified_ts BIGINT NOT NULL,
	expires_ts BIGINT NOT NULL,
	-- Created without an access token, or by a guest or suspended account;
	-- only these count towards the global limit.
	anonymous BOOLEAN NOT NULL,
	-- The creator of a session that is not anonymous, for the per-user limit.
	user_id TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS userapi_rendezvous_sessions_expires_idx
	ON userapi_rendezvous_sessions(expires_ts);

CREATE INDEX IF NOT EXISTS userapi_rendezvous_sessions_user_idx
	ON userapi_rendezvous_sessions(user_id, expires_ts);

-- The transaction IDs of successful MSC4388 writes, so that a retry gets the
-- same sequence token.
CREATE TABLE IF NOT EXISTS userapi_rendezvous_txns (
	session_id TEXT NOT NULL,
	txn_id TEXT NOT NULL,
	sequence BIGINT NOT NULL,
	expires_ts BIGINT NOT NULL,
	PRIMARY KEY (session_id, txn_id)
);

CREATE INDEX IF NOT EXISTS userapi_rendezvous_txns_expires_idx
	ON userapi_rendezvous_txns(expires_ts);
`

const insertRendezvousSessionSQL = "" +
	"INSERT INTO userapi_rendezvous_sessions (session_id, kind, data, content_type, sequence, last_modified_ts, expires_ts, anonymous, user_id)" +
	" VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)"

const countActiveRendezvousSessionsSQL = "" +
	"SELECT COUNT(*) FROM userapi_rendezvous_sessions WHERE anonymous AND expires_ts > $1"

const countActiveUserRendezvousSessionsSQL = "" +
	"SELECT COUNT(*) FROM userapi_rendezvous_sessions WHERE user_id = $1 AND NOT anonymous AND expires_ts > $2"

const selectRendezvousSessionSQL = "" +
	"SELECT data, content_type, sequence, last_modified_ts, expires_ts, anonymous, user_id FROM userapi_rendezvous_sessions" +
	" WHERE session_id = $1 AND kind = $2 AND expires_ts > $3"

// The compare-and-swap: the row lock taken by UPDATE serializes concurrent
// writers, and the loser re-evaluates the sequence after the winner commits.
const updateRendezvousSessionSQL = "" +
	"UPDATE userapi_rendezvous_sessions SET data = $1, content_type = $2, sequence = sequence + 1, last_modified_ts = $3" +
	" WHERE session_id = $4 AND kind = $5 AND sequence = $6 AND expires_ts > $3" +
	" RETURNING sequence, expires_ts"

const deleteRendezvousSessionSQL = "" +
	"DELETE FROM userapi_rendezvous_sessions WHERE session_id = $1 AND kind = $2 RETURNING expires_ts"

const deleteRendezvousSessionTxnsSQL = "" +
	"DELETE FROM userapi_rendezvous_txns WHERE session_id = $1"

const selectRendezvousTxnSQL = "" +
	"SELECT sequence FROM userapi_rendezvous_txns WHERE session_id = $1 AND txn_id = $2"

const insertRendezvousTxnSQL = "" +
	"INSERT INTO userapi_rendezvous_txns (session_id, txn_id, sequence, expires_ts) VALUES ($1, $2, $3, $4)" +
	" ON CONFLICT (session_id, txn_id) DO NOTHING"

const deleteRendezvousTxnsUpToSQL = "" +
	"DELETE FROM userapi_rendezvous_txns WHERE session_id = $1 AND sequence <= $2"

const deleteExpiredRendezvousSessionsSQL = "" +
	"DELETE FROM userapi_rendezvous_sessions WHERE expires_ts <= $1"

const deleteExpiredRendezvousTxnsSQL = "" +
	"DELETE FROM userapi_rendezvous_txns WHERE expires_ts <= $1"

type rendezvousStatements struct {
	insertSessionStmt         *sql.Stmt
	countActiveStmt           *sql.Stmt
	countActiveUserStmt       *sql.Stmt
	selectSessionStmt         *sql.Stmt
	updateSessionStmt         *sql.Stmt
	deleteSessionStmt         *sql.Stmt
	deleteSessionTxnsStmt     *sql.Stmt
	selectTxnStmt             *sql.Stmt
	insertTxnStmt             *sql.Stmt
	deleteTxnsUpToStmt        *sql.Stmt
	deleteExpiredSessionsStmt *sql.Stmt
	deleteExpiredTxnsStmt     *sql.Stmt
}

func NewPostgresRendezvousTable(db *sql.DB) (tables.RendezvousTable, error) {
	s := &rendezvousStatements{}
	if _, err := db.Exec(rendezvousSchema); err != nil {
		return nil, err
	}
	return s, sqlutil.StatementList{
		{&s.insertSessionStmt, insertRendezvousSessionSQL},
		{&s.countActiveStmt, countActiveRendezvousSessionsSQL},
		{&s.countActiveUserStmt, countActiveUserRendezvousSessionsSQL},
		{&s.selectSessionStmt, selectRendezvousSessionSQL},
		{&s.updateSessionStmt, updateRendezvousSessionSQL},
		{&s.deleteSessionStmt, deleteRendezvousSessionSQL},
		{&s.deleteSessionTxnsStmt, deleteRendezvousSessionTxnsSQL},
		{&s.selectTxnStmt, selectRendezvousTxnSQL},
		{&s.insertTxnStmt, insertRendezvousTxnSQL},
		{&s.deleteTxnsUpToStmt, deleteRendezvousTxnsUpToSQL},
		{&s.deleteExpiredSessionsStmt, deleteExpiredRendezvousSessionsSQL},
		{&s.deleteExpiredTxnsStmt, deleteExpiredRendezvousTxnsSQL},
	}.Prepare(db)
}

func (s *rendezvousStatements) InsertRendezvousSession(ctx context.Context, txn *sql.Tx, session *api.RendezvousSession) error {
	_, err := sqlutil.TxStmt(txn, s.insertSessionStmt).ExecContext(ctx,
		session.ID, string(session.Kind), session.Data, session.ContentType,
		session.Sequence, session.LastModified, session.Expires, session.Anonymous, session.UserID,
	)
	return err
}

func (s *rendezvousStatements) CountActiveRendezvousSessions(ctx context.Context, txn *sql.Tx, now spec.Timestamp) (int, error) {
	var count int
	err := sqlutil.TxStmt(txn, s.countActiveStmt).QueryRowContext(ctx, now).Scan(&count)
	return count, err
}

func (s *rendezvousStatements) CountActiveUserRendezvousSessions(ctx context.Context, txn *sql.Tx, userID string, now spec.Timestamp) (int, error) {
	var count int
	err := sqlutil.TxStmt(txn, s.countActiveUserStmt).QueryRowContext(ctx, userID, now).Scan(&count)
	return count, err
}

func (s *rendezvousStatements) SelectRendezvousSession(ctx context.Context, txn *sql.Tx, id string, kind api.RendezvousKind, now spec.Timestamp) (*api.RendezvousSession, error) {
	session := &api.RendezvousSession{ID: id, Kind: kind}
	err := sqlutil.TxStmt(txn, s.selectSessionStmt).QueryRowContext(ctx, id, string(kind), now).Scan(
		&session.Data, &session.ContentType, &session.Sequence, &session.LastModified, &session.Expires, &session.Anonymous, &session.UserID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (s *rendezvousStatements) UpdateRendezvousSession(ctx context.Context, txn *sql.Tx, id string, kind api.RendezvousKind, expectedSequence int64, data []byte, contentType string, now spec.Timestamp) (*api.RendezvousSession, error) {
	session := &api.RendezvousSession{ID: id, Kind: kind, ContentType: contentType, LastModified: now}
	err := sqlutil.TxStmt(txn, s.updateSessionStmt).QueryRowContext(ctx,
		data, contentType, now, id, string(kind), expectedSequence,
	).Scan(&session.Sequence, &session.Expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (s *rendezvousStatements) DeleteRendezvousSession(ctx context.Context, txn *sql.Tx, id string, kind api.RendezvousKind, now spec.Timestamp) (bool, error) {
	var expires spec.Timestamp
	err := sqlutil.TxStmt(txn, s.deleteSessionStmt).QueryRowContext(ctx, id, string(kind)).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = sqlutil.TxStmt(txn, s.deleteSessionTxnsStmt).ExecContext(ctx, id); err != nil {
		return false, err
	}
	return expires > now, nil
}

func (s *rendezvousStatements) SelectRendezvousTxn(ctx context.Context, txn *sql.Tx, id, txnID string) (int64, bool, error) {
	var sequence int64
	err := sqlutil.TxStmt(txn, s.selectTxnStmt).QueryRowContext(ctx, id, txnID).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return sequence, true, nil
}

func (s *rendezvousStatements) InsertRendezvousTxn(ctx context.Context, txn *sql.Tx, id, txnID string, sequence int64, expires spec.Timestamp) error {
	_, err := sqlutil.TxStmt(txn, s.insertTxnStmt).ExecContext(ctx, id, txnID, sequence, expires)
	return err
}

func (s *rendezvousStatements) DeleteRendezvousTxnsUpTo(ctx context.Context, txn *sql.Tx, id string, sequence int64) error {
	_, err := sqlutil.TxStmt(txn, s.deleteTxnsUpToStmt).ExecContext(ctx, id, sequence)
	return err
}

func (s *rendezvousStatements) DeleteExpiredRendezvousSessions(ctx context.Context, txn *sql.Tx, now spec.Timestamp) error {
	if _, err := sqlutil.TxStmt(txn, s.deleteExpiredSessionsStmt).ExecContext(ctx, now); err != nil {
		return err
	}
	_, err := sqlutil.TxStmt(txn, s.deleteExpiredTxnsStmt).ExecContext(ctx, now)
	return err
}
