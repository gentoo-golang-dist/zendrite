// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/storage"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// A join that trips the MSC3706 state-epoch guard after a partial-state resync
// must land in the room's current state snapshot, not only in the output delta.
// Otherwise the joiner's messages are soft-failed with "not in room" while
// clients list them as joined (#535).
func TestStateEpochGuardKeepsJoinInCurrentState(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	charlie := test.NewUser(t)
	dave := test.NewUser(t)
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
		defer closeRig()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches) //nolint:contextcheck
		require.NoError(t, err)

		natsInstance := jetstream.NATSInstance{}
		jsCtx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsCtx, &cfg.Global.JetStream)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))

		// Bob and Charlie are members the resync fetched authoritatively: the
		// room's current state holds them, but no forward extremity's DAG
		// state does.
		bobJoin := room.CreateEvent(t, bob, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(bob.ID))
		charlieJoin := room.CreateEvent(t, charlie, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(charlie.ID))
		resyncOnly := []*types.HeaderedEvent{bobJoin, charlieJoin}
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindOutlier, resyncOnly, "test", "test", "test", nil, false))

		roomInfo, err := db.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		resyncEntries, err := db.StateEntriesForEventIDs(ctx, []string{bobJoin.EventID(), charlieJoin.EventID()}, true)
		require.NoError(t, err)
		require.Len(t, resyncEntries, 2)

		updater, err := db.GetRoomUpdater(ctx, roomInfo)
		require.NoError(t, err)
		blocks, err := updater.StateBlockNIDs(ctx, []types.StateSnapshotNID{updater.CurrentStateSnapshotNID()})
		require.NoError(t, err)
		resyncStateNID, err := updater.AddState(ctx, roomInfo.RoomNID, blocks[0].StateBlockNIDs, resyncEntries)
		require.NoError(t, err)
		latest := updater.LatestEvents()
		var lastEventNID types.EventNID
		for _, l := range latest {
			lastEventNID = max(lastEventNID, l.EventNID)
		}
		require.NoError(t, updater.SetLatestEvents(roomInfo.RoomNID, latest, lastEventNID, resyncStateNID))
		require.NoError(t, updater.UpdateResyncStateNID(roomInfo.RoomNID, resyncStateNID))
		require.NoError(t, updater.Commit())

		// Dave's join builds on DAG state without Bob and Charlie, so its
		// delta drops both of them and trips the collateral-member guard.
		daveJoin := room.CreateAndInsert(t, dave, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(dave.ID))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{daveJoin}, "test", "test", "test", nil, false))

		msg := room.CreateAndInsert(t, dave, "m.room.message", map[string]any{"body": "hello", "msgtype": "m.text"})
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{msg}, "test", "test", "test", nil, false))

		roomInfo, err = db.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		latestIDs, _, _, err := db.LatestEventIDs(ctx, roomInfo.RoomNID)
		require.NoError(t, err)
		require.True(t, slices.Contains(latestIDs, msg.EventID()), "dave's message must not be soft-failed, latest events: %v", latestIDs)

		// The guard still preserves the resync-authoritative members.
		for _, user := range []*test.User{bob, charlie, dave} {
			ev, err := db.GetStateEvent(ctx, room.ID, spec.MRoomMember, user.ID)
			require.NoError(t, err)
			require.NotNil(t, ev, "%s must be in the current state", user.ID)
			membership, err := ev.Membership()
			require.NoError(t, err)
			require.Equal(t, spec.Join, membership)
		}
	})
}
