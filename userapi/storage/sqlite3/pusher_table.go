// Copyright 2024 New Vector Ltd.
// Copyright 2021 Dan Peleg <dan@globekeeper.com>
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sqlite3

import (
	"context"
	"database/sql"
	"encoding/json"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/userapi/api"
	"codefloe.com/pat-s/zendrite/userapi/storage/tables"
)

// See https://matrix.org/docs/spec/client_server/r0.6.1#get-matrix-client-r0-pushers
const pushersSchema = `
CREATE TABLE IF NOT EXISTS userapi_pushers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	-- The Matrix user ID localpart for this pusher
	localpart TEXT NOT NULL,
	server_name TEXT NOT NULL,
	session_id BIGINT DEFAULT NULL,
	profile_tag TEXT,
	kind TEXT NOT NULL,
	app_id TEXT NOT NULL,
	app_display_name TEXT NOT NULL,
	device_display_name TEXT NOT NULL,
	pushkey TEXT NOT NULL,
	pushkey_ts_ms BIGINT NOT NULL DEFAULT 0,
	lang TEXT NOT NULL,
	data TEXT NOT NULL
);

-- For faster deleting by app_id, pushkey pair.
CREATE INDEX IF NOT EXISTS userapi_pusher_app_id_pushkey_idx ON userapi_pushers(app_id, pushkey);

-- For faster retrieving by localpart.
CREATE INDEX IF NOT EXISTS userapi_pusher_localpart_idx ON userapi_pushers(localpart, server_name);

-- Pushkey must be unique for a given user and app.
CREATE UNIQUE INDEX IF NOT EXISTS userapi_pusher_app_id_pushkey_localpart_idx ON userapi_pushers(app_id, pushkey, localpart, server_name);
`

const insertPusherSQL = "" +
	"INSERT INTO userapi_pushers (localpart, server_name, session_id, pushkey, pushkey_ts_ms, kind, app_id, app_display_name, device_display_name, profile_tag, lang, data, enabled, device_id)" +
	" VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)" +
	" ON CONFLICT (app_id, pushkey, localpart, server_name) DO UPDATE SET pushkey_ts_ms = $5, kind = $6, app_display_name = $8, device_display_name = $9, profile_tag = $10, lang = $11, data = $12, enabled = $13," +
	// MSC3881: as in Synapse, a pusher stays with the device that registered
	// it when another device updates it, e.g. to disable it. Pushers without
	// a device, registered before MSC3881, take the updating one.
	" session_id = CASE WHEN userapi_pushers.device_id IS NULL THEN excluded.session_id ELSE userapi_pushers.session_id END," +
	" device_id = COALESCE(userapi_pushers.device_id, excluded.device_id)"

const selectPushersSQL = "" +
	"SELECT session_id, pushkey, pushkey_ts_ms, kind, app_id, app_display_name, device_display_name, profile_tag, lang, data, enabled, device_id FROM userapi_pushers WHERE localpart = $1 AND server_name = $2"

const deletePusherSQL = "" +
	"DELETE FROM userapi_pushers WHERE app_id = $1 AND pushkey = $2 AND localpart = $3 AND server_name = $4"

const deletePushersByAppIdAndPushKeySQL = "" +
	"DELETE FROM userapi_pushers WHERE app_id = $1 AND pushkey = $2 AND NOT (localpart = $3 AND server_name = $4)"

func NewSQLitePusherTable(ctx context.Context, db *sql.DB) (tables.PusherTable, error) {
	s := &pushersStatements{}
	_, err := db.Exec(pushersSchema)
	if err != nil {
		return nil, err
	}
	m := sqlutil.NewMigrator(db)
	m.AddMigrations(sqlutil.Migration{
		Version: "userapi: add pusher enabled and device ID",
		Up:      tables.AddPusherEnabledDeviceID,
	})
	if err = m.Up(ctx); err != nil {
		return nil, err
	}
	return s, sqlutil.StatementList{
		{&s.insertPusherStmt, insertPusherSQL},
		{&s.selectPushersStmt, selectPushersSQL},
		{&s.deletePusherStmt, deletePusherSQL},
		{&s.deletePushersByAppIdAndPushKeyStmt, deletePushersByAppIdAndPushKeySQL},
	}.Prepare(db)
}

type pushersStatements struct {
	insertPusherStmt                   *sql.Stmt
	selectPushersStmt                  *sql.Stmt
	deletePusherStmt                   *sql.Stmt
	deletePushersByAppIdAndPushKeyStmt *sql.Stmt
}

// insertPusher creates a new pusher.
// Returns an error if the user already has a pusher with the given pusher pushkey.
// Returns nil error success.
func (s *pushersStatements) InsertPusher(
	ctx context.Context, txn *sql.Tx, session_id int64,
	pushkey string, pushkeyTS int64, kind api.PusherKind, appid, appdisplayname, devicedisplayname, profiletag, lang, data,
	localpart string, serverName spec.ServerName, enabled bool, deviceID string,
) error {
	insertPusherStmt := sqlutil.TxStmt(txn, s.insertPusherStmt)
	_, err := insertPusherStmt.ExecContext(ctx, localpart, serverName, session_id, pushkey, pushkeyTS, kind, appid, appdisplayname, devicedisplayname, profiletag, lang, data, enabled, sql.NullString{String: deviceID, Valid: deviceID != ""})
	return err
}

func (s *pushersStatements) SelectPushers(
	ctx context.Context, txn *sql.Tx,
	localpart string, serverName spec.ServerName,
) ([]api.Pusher, error) {
	pushers := []api.Pusher{}
	rows, err := s.selectPushersStmt.QueryContext(ctx, localpart, serverName)
	if err != nil {
		return pushers, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectPushers: rows.close() failed")

	for rows.Next() {
		var pusher api.Pusher
		var data []byte
		var enabled bool
		var deviceID sql.NullString
		err = rows.Scan(
			&pusher.SessionID,
			&pusher.PushKey,
			&pusher.PushKeyTS,
			&pusher.Kind,
			&pusher.AppID,
			&pusher.AppDisplayName,
			&pusher.DeviceDisplayName,
			&pusher.ProfileTag,
			&pusher.Language,
			&data,
			&enabled,
			&deviceID)
		if err != nil {
			return pushers, err
		}
		err := json.Unmarshal(data, &pusher.Data)
		if err != nil {
			return pushers, err
		}
		pusher.Enabled = &enabled
		pusher.DeviceID = deviceID.String
		pushers = append(pushers, pusher)
	}

	logrus.Tracef("Database returned %d pushers", len(pushers))
	return pushers, rows.Err()
}

// deletePusher removes a single pusher by pushkey and user localpart.
func (s *pushersStatements) DeletePusher(
	ctx context.Context, txn *sql.Tx, appid, pushkey,
	localpart string, serverName spec.ServerName,
) error {
	deletePusherStmt := sqlutil.TxStmt(txn, s.deletePusherStmt)
	_, err := deletePusherStmt.ExecContext(ctx, appid, pushkey, localpart, serverName)
	return err
}

// DeletePushers removes the pushers with the given app ID and pushkey of
// every user except the given one.
func (s *pushersStatements) DeletePushers(
	ctx context.Context, txn *sql.Tx, appid, pushkey,
	exceptLocalpart string, exceptServerName spec.ServerName,
) error {
	deletePushersByAppIdAndPushKeyStmt := sqlutil.TxStmt(txn, s.deletePushersByAppIdAndPushKeyStmt)
	_, err := deletePushersByAppIdAndPushKeyStmt.ExecContext(ctx, appid, pushkey, exceptLocalpart, exceptServerName)
	return err
}
