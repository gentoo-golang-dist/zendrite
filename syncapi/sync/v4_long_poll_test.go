// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matrix-org/util"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/notifier"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// A long poll that wakes up without anything to send loops inside the handler.
// Every pass opens database snapshots, and they used to be released by defers
// that only ran when the handler returned, so each idle wake-up kept another
// transaction, and its pooled connection, open. A few busy clients exhausted
// the shared pool, after which every query in the process, access token
// checks included, waited for a connection until its request was canceled.
func TestSlidingSyncLongPollReleasesConnectionsEachPass(t *testing.T) {
	test.WithAllDatabases(t, testSlidingSyncLongPollReleasesConnectionsEachPass)
}

func testSlidingSyncLongPollReleasesConnectionsEachPass(t *testing.T, dbType test.DBType) {
	const (
		poolSize = 4
		passes   = 3 * poolSize
	)
	cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
	defer closeDB()
	cfg.Global.DatabaseOptions.MaxOpenConnections = poolSize
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	realDB, err := storage.NewSyncServerDatasource(processCtx.Context(), cm, &cfg.SyncAPI.Database)
	require.NoError(t, err)

	ctx := t.Context()
	device := &userapi.Device{UserID: "@alice:localhost", ID: "DEVICE"}
	connectionKey, err := realDB.GetOrCreateConnection(ctx, device.UserID, device.ID, "default")
	require.NoError(t, err)
	acked, err := realDB.CreateConnectionPosition(ctx, connectionKey)
	require.NoError(t, err)

	rp := &RequestPool{
		db:       realDB,
		cfg:      &config.SyncAPI{Matrix: &config.Global{}},
		lastseen: &sync.Map{},
		presence: &sync.Map{},
		Notifier: notifier.NewNotifier(nil),
		rsAPI:    noPartialStateRoomserver{},
	}
	rp.lastseen.Store(device.UserID+device.ID, struct{}{})

	hook := logrustest.NewGlobal()
	defer hook.Reset()
	waits := func() int {
		n := 0
		for _, entry := range hook.AllEntries() {
			if entry.Message == "[V4_SYNC] Entering long poll wait" {
				n++
			}
		}
		return n
	}

	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	body, err := json.Marshal(map[string]any{
		"pos":     types.NewSlidingSyncStreamToken(acked, types.StreamingToken{}).String(),
		"timeout": 60000,
	})
	require.NoError(t, err)
	done := make(chan util.JSONResponse, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/_matrix/client/unstable/org.matrix.simplified_msc3575/sync", strings.NewReader(string(body))).WithContext(reqCtx)
		done <- rp.OnIncomingSyncRequestV4(req, device)
	}()

	// Wake the request up repeatedly with nothing for it to send.
	for pass := 1; pass <= passes; pass++ {
		require.Eventually(t, func() bool { return waits() >= pass }, 5*time.Second, 5*time.Millisecond,
			"the long poll stopped looping after %d passes", pass-1)
		rp.Notifier.OnNewNotificationData(device.UserID, types.StreamingToken{NotificationDataPosition: types.StreamPosition(pass)})
	}
	require.Eventually(t, func() bool { return waits() > passes }, 5*time.Second, 5*time.Millisecond)

	// The request is idle in its long poll; the pool must not be held by it.
	queryCtx, queryCancel := context.WithTimeout(ctx, 2*time.Second)
	defer queryCancel()
	snapshot, err := realDB.NewDatabaseSnapshot(queryCtx)
	require.NoError(t, err, "an idle long poll must not hold the connection pool")
	require.NoError(t, snapshot.Rollback())

	cancel(nil)
	select {
	case res := <-done:
		assert.Equal(t, http.StatusOK, res.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not return after its client went away")
	}
}
