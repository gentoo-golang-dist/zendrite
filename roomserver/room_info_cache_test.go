// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/storage"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// missingRoomNIDCache never holds a room NID but keeps room versions, as the
// real cache does while a set of the NID is still buffered or was dropped.
// Like the real cache it refuses to change a stored room version.
type missingRoomNIDCache struct {
	caching.RoomServerCaches
	mu       sync.Mutex
	versions map[string]gomatrixserverlib.RoomVersion
	changed  []string
}

func (c *missingRoomNIDCache) GetRoomServerRoomNID(string) (types.RoomNID, bool) { return 0, false }

func (c *missingRoomNIDCache) GetRoomVersion(roomID string) (gomatrixserverlib.RoomVersion, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.versions[roomID]
	return v, ok
}

func (c *missingRoomNIDCache) StoreRoomVersion(roomID string, version gomatrixserverlib.RoomVersion) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.versions[roomID]; ok && old != version {
		c.changed = append(c.changed, roomID+": "+string(old)+" -> "+string(version))
		return
	}
	c.versions[roomID] = version
}

// An event other than the create event carries no room version. Looking up
// its room while the room NID is not cached must keep the cached version, not
// overwrite it with an empty one, which panics on the immutable cache.
func TestGetOrCreateRoomInfoKeepsCachedVersion(t *testing.T) {
	alice := test.NewUser(t)
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
		defer closeRig()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		cache := &missingRoomNIDCache{
			RoomServerCaches: caching.NewRistrettoCache(8*1024*1024, time.Hour, caching.DisableMetrics),
			versions:         map[string]gomatrixserverlib.RoomVersion{},
		}
		db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, cache) //nolint:contextcheck
		require.NoError(t, err)

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat), test.RoomVersion(gomatrixserverlib.RoomVersionV10))
		events := room.Events()
		require.Equal(t, spec.MRoomCreate, events[0].Type())

		created, err := db.GetOrCreateRoomInfo(ctx, events[0].PDU)
		require.NoError(t, err)
		require.Equal(t, gomatrixserverlib.RoomVersionV10, created.RoomVersion)

		for _, ev := range events[1:] {
			info, err := db.GetOrCreateRoomInfo(ctx, ev.PDU)
			require.NoError(t, err)
			require.Equal(t, created.RoomNID, info.RoomNID)
			require.Equal(t, gomatrixserverlib.RoomVersionV10, info.RoomVersion)
		}
		require.Empty(t, cache.changed, "the cached room version was overwritten")
	})
}
