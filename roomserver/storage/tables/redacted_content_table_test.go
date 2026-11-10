// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package tables_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/storage/postgres"
	"codefloe.com/pat-s/zendrite/roomserver/storage/sqlite3"
	"codefloe.com/pat-s/zendrite/roomserver/storage/tables"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
)

func mustCreateRedactedContentTable(t *testing.T, dbType test.DBType) (tab tables.RedactedContent, close func()) {
	t.Helper()
	connStr, close := test.PrepareDBConnectionString(t, dbType)
	db, err := sqlutil.Open(&config.DatabaseOptions{
		ConnectionString: config.DataSource(connStr),
	}, sqlutil.NewExclusiveWriter())
	require.NoError(t, err)
	switch dbType {
	case test.DBTypePostgres:
		require.NoError(t, postgres.CreateRedactedContentTable(db))
		tab, err = postgres.PrepareRedactedContentTable(db)
	case test.DBTypeSQLite:
		require.NoError(t, sqlite3.CreateRedactedContentTable(db))
		tab, err = sqlite3.PrepareRedactedContentTable(db)
	}
	require.NoError(t, err)
	return tab, close
}

func TestRedactedContentTable(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		tab, close := mustCreateRedactedContentTable(t, dbType)
		defer close()

		content, err := tab.SelectRedactedContent(ctx, nil, 1, 0)
		require.NoError(t, err)
		assert.Nil(t, content)

		require.NoError(t, tab.InsertRedactedContent(ctx, nil, 1, 10, []byte(`{"content":{"body":"first"}}`), 1000))
		// A second redaction keeps the content recorded first.
		require.NoError(t, tab.InsertRedactedContent(ctx, nil, 1, 10, []byte(`{"content":{}}`), 2000))
		require.NoError(t, tab.InsertRedactedContent(ctx, nil, 2, 10, []byte(`{"content":{"body":"second"}}`), 3000))

		content, err = tab.SelectRedactedContent(ctx, nil, 1, 1000)
		require.NoError(t, err)
		assert.JSONEq(t, `{"content":{"body":"first"}}`, string(content))
		// Content redacted before the cutoff is no longer served, even before
		// it is deleted.
		content, err = tab.SelectRedactedContent(ctx, nil, 1, 1001)
		require.NoError(t, err)
		assert.Nil(t, content)

		removed, err := tab.DeleteRedactedContentBefore(ctx, nil, 3000)
		require.NoError(t, err)
		assert.Equal(t, int64(1), removed)
		content, err = tab.SelectRedactedContent(ctx, nil, 1, 0)
		require.NoError(t, err)
		assert.Nil(t, content)
		content, err = tab.SelectRedactedContent(ctx, nil, 2, 0)
		require.NoError(t, err)
		assert.JSONEq(t, `{"content":{"body":"second"}}`, string(content))
	})
}
