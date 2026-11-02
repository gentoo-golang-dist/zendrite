// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package postgres

import (
	"context"
	"database/sql"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/userapi/api"
	"codefloe.com/pat-s/zendrite/userapi/storage/tables"
)

const threadSubscriptionsSchema = `
-- Stores MSC4306 thread subscriptions. Unsubscriptions keep their row, so
-- that sliding sync can report them and later automatic subscriptions can be
-- ordered against them.
CREATE TABLE IF NOT EXISTS userapi_thread_subscriptions (
	localpart TEXT NOT NULL,
	server_name TEXT NOT NULL,
	room_id TEXT NOT NULL,
	-- The event ID of the thread root
	thread_root_id TEXT NOT NULL,
	subscribed BOOLEAN NOT NULL,
	automatic BOOLEAN NOT NULL,
	-- The position of the latest change in the thread subscriptions stream
	stream_id BIGINT NOT NULL,
	-- The sync stream position of the room's latest event at unsubscription
	unsubscribed_at BIGINT NOT NULL DEFAULT 0,
	PRIMARY KEY (localpart, server_name, room_id, thread_root_id)
);
CREATE INDEX IF NOT EXISTS userapi_thread_subscriptions_stream_idx
	ON userapi_thread_subscriptions (localpart, server_name, stream_id);
CREATE INDEX IF NOT EXISTS userapi_thread_subscriptions_thread_idx
	ON userapi_thread_subscriptions (room_id, thread_root_id) WHERE subscribed;
`

// EXCLUSIVE mode blocks other writers but not readers.
const lockThreadSubscriptionsSQL = "" +
	"LOCK TABLE userapi_thread_subscriptions IN EXCLUSIVE MODE"

const selectMaxThreadSubscriptionStreamIDSQL = "" +
	"SELECT COALESCE(MAX(stream_id), 0) FROM userapi_thread_subscriptions"

const selectThreadSubscriptionSQL = "" +
	"SELECT subscribed, automatic, stream_id, unsubscribed_at FROM userapi_thread_subscriptions" +
	" WHERE localpart = $1 AND server_name = $2 AND room_id = $3 AND thread_root_id = $4"

const upsertThreadSubscriptionSQL = "" +
	"INSERT INTO userapi_thread_subscriptions" +
	" (localpart, server_name, room_id, thread_root_id, subscribed, automatic, stream_id, unsubscribed_at)" +
	" VALUES ($1, $2, $3, $4, $5, $6, $7, $8)" +
	" ON CONFLICT (localpart, server_name, room_id, thread_root_id) DO UPDATE SET" +
	" subscribed = excluded.subscribed, automatic = excluded.automatic," +
	" stream_id = excluded.stream_id, unsubscribed_at = excluded.unsubscribed_at"

// The latest changes win when there are more than the limit.
const selectThreadSubscriptionUpdatesSQL = "" +
	"SELECT stream_id, room_id, thread_root_id, subscribed, automatic FROM (" +
	" SELECT stream_id, room_id, thread_root_id, subscribed, automatic FROM userapi_thread_subscriptions" +
	" WHERE localpart = $1 AND server_name = $2 AND stream_id > $3 AND stream_id <= $4" +
	" ORDER BY stream_id DESC LIMIT $5" +
	") AS updates ORDER BY stream_id ASC"

const selectThreadSubscribersSQL = "" +
	"SELECT localpart, server_name FROM userapi_thread_subscriptions" +
	" WHERE room_id = $1 AND thread_root_id = $2 AND subscribed"

type threadSubscriptionsStatements struct {
	lockStmt       *sql.Stmt
	selectMaxStmt  *sql.Stmt
	selectStmt     *sql.Stmt
	upsertStmt     *sql.Stmt
	selectUpdates  *sql.Stmt
	selectSubsStmt *sql.Stmt
}

func NewPostgresThreadSubscriptionsTable(db *sql.DB) (tables.ThreadSubscriptionsTable, error) {
	s := &threadSubscriptionsStatements{}
	if _, err := db.Exec(threadSubscriptionsSchema); err != nil {
		return nil, err
	}
	return s, sqlutil.StatementList{
		{&s.lockStmt, lockThreadSubscriptionsSQL},
		{&s.selectMaxStmt, selectMaxThreadSubscriptionStreamIDSQL},
		{&s.selectStmt, selectThreadSubscriptionSQL},
		{&s.upsertStmt, upsertThreadSubscriptionSQL},
		{&s.selectUpdates, selectThreadSubscriptionUpdatesSQL},
		{&s.selectSubsStmt, selectThreadSubscribersSQL},
	}.Prepare(db)
}

func (s *threadSubscriptionsStatements) LockForWrite(ctx context.Context, txn *sql.Tx) error {
	_, err := sqlutil.TxStmt(txn, s.lockStmt).ExecContext(ctx)
	return err
}

func (s *threadSubscriptionsStatements) SelectMaxStreamID(ctx context.Context, txn *sql.Tx) (id int64, err error) {
	err = sqlutil.TxStmt(txn, s.selectMaxStmt).QueryRowContext(ctx).Scan(&id)
	return
}

func (s *threadSubscriptionsStatements) SelectThreadSubscription(
	ctx context.Context, txn *sql.Tx, localpart string, serverName spec.ServerName, roomID, threadRootID string,
) (*tables.ThreadSubscriptionRow, error) {
	var row tables.ThreadSubscriptionRow
	err := sqlutil.TxStmt(txn, s.selectStmt).QueryRowContext(ctx, localpart, serverName, roomID, threadRootID).
		Scan(&row.Subscribed, &row.Automatic, &row.StreamID, &row.UnsubscribedAt)
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *threadSubscriptionsStatements) UpsertThreadSubscription(
	ctx context.Context, txn *sql.Tx, localpart string, serverName spec.ServerName, roomID, threadRootID string,
	row *tables.ThreadSubscriptionRow,
) error {
	_, err := sqlutil.TxStmt(txn, s.upsertStmt).ExecContext(
		ctx, localpart, serverName, roomID, threadRootID, row.Subscribed, row.Automatic, row.StreamID, row.UnsubscribedAt,
	)
	return err
}

func (s *threadSubscriptionsStatements) SelectThreadSubscriptionUpdates(
	ctx context.Context, txn *sql.Tx, localpart string, serverName spec.ServerName, from, to int64, limit int,
) ([]api.ThreadSubscriptionUpdate, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectUpdates).QueryContext(ctx, localpart, serverName, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectThreadSubscriptionUpdates: rows.close() failed")
	var updates []api.ThreadSubscriptionUpdate
	for rows.Next() {
		var u api.ThreadSubscriptionUpdate
		if err = rows.Scan(&u.StreamID, &u.RoomID, &u.ThreadRootID, &u.Subscribed, &u.Automatic); err != nil {
			return nil, err
		}
		updates = append(updates, u)
	}
	return updates, rows.Err()
}

func (s *threadSubscriptionsStatements) SelectThreadSubscribers(
	ctx context.Context, txn *sql.Tx, roomID, threadRootID string,
) ([]string, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectSubsStmt).QueryContext(ctx, roomID, threadRootID)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectThreadSubscribers: rows.close() failed")
	var userIDs []string
	for rows.Next() {
		var localpart, serverName string
		if err = rows.Scan(&localpart, &serverName); err != nil {
			return nil, err
		}
		userIDs = append(userIDs, "@"+localpart+":"+serverName)
	}
	return userIDs, rows.Err()
}
