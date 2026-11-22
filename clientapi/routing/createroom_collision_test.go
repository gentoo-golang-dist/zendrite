// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"net/http"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/appservice"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
	uapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// TestCreateRoomSameCreateEvent checks that two rooms created by the same
// user with the same content and timestamp get their own room IDs, though
// the room ID of a version 12 room is the hash of its create event (Sytest
// "GET /publicRooms lists rooms" creates five rooms at once).
func TestCreateRoomSameCreateEvent(t *testing.T) {
	alice := test.NewUser(t)
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := t.Context()
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		asAPI := appservice.NewInternalAPI(processCtx, cfg, &natsInstance, userAPI, rsAPI)

		localpart, serverName, err := gomatrixserverlib.SplitID('@', alice.ID)
		require.NoError(t, err)
		require.NoError(t, userAPI.PerformAccountCreation(ctx, &uapi.PerformAccountCreationRequest{
			AccountType: uapi.AccountTypeUser,
			Localpart:   localpart,
			ServerName:  serverName,
			Password:    "someRandomPassword",
		}, &uapi.PerformAccountCreationResponse{}))
		device := &uapi.Device{UserID: alice.ID}

		evTime := time.Now()
		roomIDs := map[string]bool{}
		for range 3 {
			resp := createRoom(ctx, createRoomRequest{
				Visibility:  "public",
				RoomVersion: gomatrixserverlib.RoomVersionV12,
			}, device, &cfg.ClientAPI, userAPI, rsAPI, asAPI, evTime)
			require.Equal(t, http.StatusOK, resp.Code, "%+v", resp.JSON)
			created, ok := resp.JSON.(createRoomResponse)
			require.True(t, ok)
			assert.False(t, roomIDs[created.RoomID], "room ID %s was given out twice", created.RoomID)
			roomIDs[created.RoomID] = true
		}
	})
}
