// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sync

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// When a sliding sync request's context ends, database/sql rolls back the
// snapshot transaction opened on it, and queries on that snapshot then fail
// with sql.ErrTxDone ("transaction has already been committed or rolled
// back"), not context.Canceled: the statements are bound to the transaction
// with tx.Stmt, which checks the transaction rather than the query's context.
// Building room data on such a snapshot used to log an error per failed query,
// such as "Failed to get timeline events", although the client was simply
// gone. It must stop quietly instead.
func TestBuildRoomDataOnSnapshotRolledBackByCancellation(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		db, err := storage.NewSyncServerDatasource(processCtx.Context(), cm, &cfg.SyncAPI.Database)
		require.NoError(t, err)

		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		snapshot, err := db.NewDatabaseSnapshot(ctx)
		require.NoError(t, err)
		defer func() { _ = snapshot.Rollback() }()

		// The client goes away.
		cancel(nil)
		if dbType == test.DBTypePostgres {
			// database/sql rolls the transaction back asynchronously; wait
			// until it has, so every query below sees it. SQLite snapshots
			// have no transaction, so their queries fail on the context.
			require.Eventually(t, func() bool {
				_, probeErr := snapshot.MaxStreamPositionForPDUs(context.Background())
				return errors.Is(probeErr, sql.ErrTxDone)
			}, 5*time.Second, 10*time.Millisecond, "the snapshot should be rolled back once its context ends")
			_, probeErr := snapshot.MaxStreamPositionForPDUs(ctx)
			require.ErrorIs(t, probeErr, sql.ErrTxDone)
			require.NotErrorIs(t, probeErr, context.Canceled, "the failure does not say the request was canceled")
		}

		hook := logrustest.NewGlobal()
		defer hook.Reset()

		const (
			roomID = "!room:localhost"
			userID = "@alice:localhost"
		)
		rp := &RequestPool{
			db:      db,
			cfg:     &config.SyncAPI{Matrix: &config.Global{}},
			rsAPI:   noPartialStateRoomserver{},
			userAPI: otkUserAPI{},
		}
		lastToken := types.StreamingToken{PDUPosition: 1}
		connState := &V4ConnectionState{PreviousStreamStates: map[string]map[string]*types.SlidingSyncStreamState{
			roomID: {"events": {RoomStatus: types.HaveSentRoomLive.String(), LastToken: lastToken.String()}},
		}}
		roomState := determineRoomStreamState(ctx, snapshot, connState, roomID, userID)

		currentPos := types.StreamingToken{PDUPosition: 5, InvitePosition: 5}
		roomData, err := rp.BuildRoomData(ctx, snapshot, roomID, userID, 10, roomState, currentPos, &lastToken, nil, false)

		if !assert.Zero(t, countLogs(hook, ""), "a request that ended must not log its rolled-back snapshot's failures") {
			for _, entry := range hook.AllEntries() {
				t.Logf("logged %s: %s (%v)", entry.Level, entry.Message, entry.Data["error"])
			}
		}
		assert.ErrorIs(t, err, context.Canceled, "the caller must be told the request ended")
		assert.Nil(t, roomData)
	})
}
