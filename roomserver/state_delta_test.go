// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/storage"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// An entry that restores an event an intermediate block overrode must be kept
// by AddState and AddStateDelta alike: dropping it because its event already
// appears in an earlier block would let the overriding entry win.
func TestAddStateDeltaKeepsOverriddenEntry(t *testing.T) {
	alice := test.NewUser(t)
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
		defer closeRig()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(8*1024*1024, time.Hour, caching.DisableMetrics)
		db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches) //nolint:contextcheck
		require.NoError(t, err)

		natsInstance := jetstream.NATSInstance{}
		jsCtx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsCtx, &cfg.Global.JetStream)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		rename := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": "join", "displayname": "Alice"}, test.WithStateKey(alice.ID))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, append(room.Events(), rename), "test", "test", "test", nil, false))

		var join string
		for _, ev := range room.Events() {
			if ev.Type() == spec.MRoomMember && ev.StateKeyEquals(alice.ID) && ev.EventID() != rename.EventID() {
				join = ev.EventID()
				break
			}
		}
		require.NotEmpty(t, join)
		entries, err := db.StateEntriesForEventIDs(ctx, []string{join, rename.EventID()}, true)
		require.NoError(t, err)
		require.Len(t, entries, 2)
		joinEntry, renameEntry := entries[0], entries[1]
		if joinEntry.EventNID > renameEntry.EventNID {
			joinEntry, renameEntry = renameEntry, joinEntry
		}
		require.Equal(t, joinEntry.StateKeyTuple, renameEntry.StateKeyTuple)

		roomInfo, err := db.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		base, err := db.AddState(ctx, roomInfo.RoomNID, nil, []types.StateEntry{joinEntry})
		require.NoError(t, err)
		overridden, err := db.AddState(ctx, roomInfo.RoomNID, blocksOf(ctx, t, db, base), []types.StateEntry{renameEntry})
		require.NoError(t, err)
		overriddenBlocks := blocksOf(ctx, t, db, overridden)
		require.Len(t, overriddenBlocks, 2)

		viaAddState, err := db.AddState(ctx, roomInfo.RoomNID, overriddenBlocks, []types.StateEntry{joinEntry})
		require.NoError(t, err)
		require.Equal(t, joinEntry.EventNID, eventAt(ctx, t, db, roomInfo, viaAddState, joinEntry.StateKeyTuple),
			"AddState keeps the restoring entry")

		viaDelta, err := db.AddStateDelta(ctx, roomInfo.RoomNID, overriddenBlocks, []types.StateEntry{joinEntry})
		require.NoError(t, err)
		require.Equal(t, joinEntry.EventNID, eventAt(ctx, t, db, roomInfo, viaDelta, joinEntry.StateKeyTuple))
	})
}

func blocksOf(ctx context.Context, t *testing.T, db storage.Database, nid types.StateSnapshotNID) []types.StateBlockNID {
	t.Helper()
	lists, err := db.StateBlockNIDs(ctx, []types.StateSnapshotNID{nid})
	require.NoError(t, err)
	require.Len(t, lists, 1)
	return lists[0].StateBlockNIDs
}

func eventAt(
	ctx context.Context, t *testing.T, db storage.Database, roomInfo *types.RoomInfo,
	nid types.StateSnapshotNID, key types.StateKeyTuple,
) types.EventNID {
	t.Helper()
	resolver := state.NewStateResolution(db, roomInfo, nil)
	entries, err := resolver.LoadStateAtSnapshot(ctx, nid)
	require.NoError(t, err)
	for _, e := range entries {
		if e.StateKeyTuple == key {
			return e.EventNID
		}
	}
	t.Fatalf("snapshot %d has no entry for %v", nid, key)
	return 0
}
