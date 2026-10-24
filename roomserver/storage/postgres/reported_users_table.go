// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package postgres

import (
	"context"
	"database/sql"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/storage/tables"
)

// User reports (MSC4260) keep both user IDs as text, so that a report
// outlives the reported user, who may also be remote.
const reportedUsersSchema = `
CREATE TABLE IF NOT EXISTS roomserver_reported_users
(
	id             BIGSERIAL PRIMARY KEY,
	target_user_id TEXT NOT NULL, -- the user being reported
	user_id        TEXT NOT NULL, -- the user reporting them
	reason         TEXT NOT NULL,
	received_ts    BIGINT NOT NULL
);`

const insertReportedUserSQL = "" +
	"INSERT INTO roomserver_reported_users (target_user_id, user_id, reason, received_ts) VALUES ($1, $2, $3, $4) RETURNING id"

const countReportedUsersSQL = "" +
	"SELECT COUNT(*) FROM roomserver_reported_users WHERE ($1 = '' OR target_user_id = $1) AND ($2 = '' OR user_id = $2)"

const selectReportedUsersDescSQL = "" +
	"SELECT id, target_user_id, user_id, reason, received_ts FROM roomserver_reported_users" +
	" WHERE ($1 = '' OR target_user_id = $1) AND ($2 = '' OR user_id = $2)" +
	" ORDER BY received_ts DESC, id DESC LIMIT $3 OFFSET $4"

const selectReportedUsersAscSQL = "" +
	"SELECT id, target_user_id, user_id, reason, received_ts FROM roomserver_reported_users" +
	" WHERE ($1 = '' OR target_user_id = $1) AND ($2 = '' OR user_id = $2)" +
	" ORDER BY received_ts ASC, id ASC LIMIT $3 OFFSET $4"

type reportedUsersStatements struct {
	insertReportedUserStmt      *sql.Stmt
	countReportedUsersStmt      *sql.Stmt
	selectReportedUsersDescStmt *sql.Stmt
	selectReportedUsersAscStmt  *sql.Stmt
}

func CreateReportedUsersTable(db *sql.DB) error {
	_, err := db.Exec(reportedUsersSchema)
	return err
}

func PrepareReportedUsersTable(db *sql.DB) (tables.ReportedUsers, error) {
	s := &reportedUsersStatements{}
	return s, sqlutil.StatementList{
		{&s.insertReportedUserStmt, insertReportedUserSQL},
		{&s.countReportedUsersStmt, countReportedUsersSQL},
		{&s.selectReportedUsersDescStmt, selectReportedUsersDescSQL},
		{&s.selectReportedUsersAscStmt, selectReportedUsersAscSQL},
	}.Prepare(db)
}

func (s *reportedUsersStatements) InsertReportedUser(
	ctx context.Context, txn *sql.Tx, targetUserID, userID, reason string,
) (reportID int64, err error) {
	err = sqlutil.TxStmt(txn, s.insertReportedUserStmt).QueryRowContext(
		ctx, targetUserID, userID, reason, spec.AsTimestamp(time.Now()),
	).Scan(&reportID)
	return
}

func (s *reportedUsersStatements) SelectReportedUsers(
	ctx context.Context, txn *sql.Tx, from, limit uint64, backwards bool, userID, targetUserID string,
) ([]api.QueryAdminUserReportsResponse, int64, error) {
	var count int64
	if err := sqlutil.TxStmt(txn, s.countReportedUsersStmt).QueryRowContext(ctx, targetUserID, userID).Scan(&count); err != nil {
		return nil, 0, err
	}
	stmt := s.selectReportedUsersAscStmt
	if backwards {
		stmt = s.selectReportedUsersDescStmt
	}
	rows, err := sqlutil.TxStmt(txn, stmt).QueryContext(ctx, targetUserID, userID, limit, from)
	if err != nil {
		return nil, 0, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectReportedUsers: failed to close rows")
	reports := []api.QueryAdminUserReportsResponse{}
	for rows.Next() {
		var report api.QueryAdminUserReportsResponse
		if err = rows.Scan(&report.ID, &report.TargetUserID, &report.UserID, &report.Reason, &report.ReceivedTS); err != nil {
			return nil, 0, err
		}
		reports = append(reports, report)
	}
	return reports, count, rows.Err()
}
