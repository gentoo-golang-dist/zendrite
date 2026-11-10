// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//go:build !wasm

package storage

import (
	"context"
	"fmt"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/internal/pushrules"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/userapi/storage/postgres"
	"codefloe.com/pat-s/zendrite/userapi/storage/shared"
	"codefloe.com/pat-s/zendrite/userapi/storage/sqlite3"
)

// NewUserDatabase opens a new Postgres or Sqlite database (based on dataSourceName scheme)
// and sets postgres connection parameters.
func NewUserDatabase(
	ctx context.Context,
	conMan *sqlutil.Connections,
	dbProperties *config.DatabaseOptions,
	serverName spec.ServerName,
	bcryptCost int,
	openIDTokenLifetimeMS int64,
	loginTokenLifetime time.Duration,
	serverNoticesLocalpart string,
	options ...Option,
) (UserDatabase, error) {
	var (
		db  *shared.Database
		err error
	)
	switch {
	case dbProperties.ConnectionString.IsSQLite():
		db, err = sqlite3.NewUserDatabase(ctx, conMan, dbProperties, serverName, bcryptCost, openIDTokenLifetimeMS, loginTokenLifetime, serverNoticesLocalpart)
	case dbProperties.ConnectionString.IsPostgres():
		db, err = postgres.NewDatabase(ctx, conMan, dbProperties, serverName, bcryptCost, openIDTokenLifetimeMS, loginTokenLifetime, serverNoticesLocalpart)
	default:
		return nil, fmt.Errorf("unexpected database type")
	}
	if err != nil {
		return nil, err
	}
	for _, option := range options {
		option(db)
	}
	return db, nil
}

// An Option configures a user database.
type Option func(*shared.Database)

// WithPushRuleOptions selects the default push rules that depend on server
// configuration.
func WithPushRuleOptions(opts pushrules.Options) Option {
	return func(db *shared.Database) { db.PushRuleOptions = opts }
}

// NewKeyDatabase opens a new Postgres or Sqlite database (base on dataSourceName) scheme)
// and sets postgres connection parameters..
func NewKeyDatabase(conMan *sqlutil.Connections, dbProperties *config.DatabaseOptions) (KeyDatabase, error) {
	switch {
	case dbProperties.ConnectionString.IsSQLite():
		return sqlite3.NewKeyDatabase(conMan, dbProperties)
	case dbProperties.ConnectionString.IsPostgres():
		return postgres.NewKeyDatabase(conMan, dbProperties)
	default:
		return nil, fmt.Errorf("unexpected database type")
	}
}
