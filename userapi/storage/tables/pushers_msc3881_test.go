// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package tables_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/userapi/storage/postgres"
	"codefloe.com/pat-s/zendrite/userapi/storage/sqlite3"
	"codefloe.com/pat-s/zendrite/userapi/storage/tables"
)

// The pushers table as servers stored it before MSC3881.
const preMSC3881PushersSchema = `
CREATE TABLE userapi_pushers (
	id %s,
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
CREATE UNIQUE INDEX userapi_pusher_app_id_pushkey_localpart_idx ON userapi_pushers(app_id, pushkey, localpart, server_name);
`

// TestAddPusherEnabledDeviceID covers the MSC3881 migration of pushers
// stored before it: they stay enabled and take the device ID of the session
// that registered them, when that device still exists.
func TestAddPusherEnabledDeviceID(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		connStr, closeDB := test.PrepareDBConnectionString(t, dbType)
		defer closeDB()
		db, err := sqlutil.Open(&config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}, nil)
		require.NoError(t, err)

		idColumn := "BIGSERIAL PRIMARY KEY"
		if dbType == test.DBTypeSQLite {
			idColumn = "INTEGER PRIMARY KEY AUTOINCREMENT"
		}
		_, err = db.ExecContext(ctx, fmt.Sprintf(preMSC3881PushersSchema, idColumn))
		require.NoError(t, err)
		switch dbType {
		case test.DBTypePostgres:
			_, err = postgres.NewPostgresDevicesTable(db, "test") //nolint:contextcheck
		case test.DBTypeSQLite:
			_, err = sqlite3.NewSQLiteDevicesTable(db, "test") //nolint:contextcheck
		}
		require.NoError(t, err)

		for _, d := range []struct {
			token, deviceID, localpart string
			session                    int64
		}{
			{"tok1", "ALICEPHONE", "alice", 11},
			{"tok2", "BOBPHONE", "bob", 12},
		} {
			_, err = db.ExecContext(ctx,
				"INSERT INTO userapi_devices (access_token, session_id, device_id, localpart, server_name, created_ts, last_seen_ts) VALUES ($1, $2, $3, $4, 'test', 0, 0)",
				d.token, d.session, d.deviceID, d.localpart)
			require.NoError(t, err)
		}
		for _, p := range []struct {
			localpart, pushkey string
			session            any
		}{
			{"alice", "alice-key", int64(11)},
			// Session 12 belongs to bob, not to carol.
			{"carol", "carol-key", int64(12)},
			// The device of session 99 is gone.
			{"dave", "dave-key", int64(99)},
			{"erin", "erin-key", int64(0)},
		} {
			_, err = db.ExecContext(ctx,
				"INSERT INTO userapi_pushers (localpart, server_name, session_id, profile_tag, kind, app_id, app_display_name, device_display_name, pushkey, lang, data) VALUES ($1, 'test', $2, '', 'http', 'app', 'App', 'Phone', $3, 'en', '{}')",
				p.localpart, p.session, p.pushkey)
			require.NoError(t, err)
		}

		var pushers tables.PusherTable
		switch dbType {
		case test.DBTypePostgres:
			pushers, err = postgres.NewPostgresPusherTable(ctx, db)
		case test.DBTypeSQLite:
			pushers, err = sqlite3.NewSQLitePusherTable(ctx, db)
		}
		require.NoError(t, err)

		want := map[string]string{"alice": "ALICEPHONE", "carol": "", "dave": "", "erin": ""}
		for localpart, deviceID := range want {
			got, err := pushers.SelectPushers(ctx, nil, localpart, "test")
			require.NoError(t, err)
			require.Len(t, got, 1, localpart)
			assert.True(t, got[0].IsEnabled(), localpart)
			require.NotNil(t, got[0].Enabled, localpart)
			assert.Equal(t, deviceID, got[0].DeviceID, localpart)
		}
	})
}
