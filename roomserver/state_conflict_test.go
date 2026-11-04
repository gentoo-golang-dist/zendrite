// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

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

// State resolution v2.1 replays conflicted events without the unconflicted state,
// so each needs the membership of its sender from its auth events. Without it
// every conflicted event fails auth, and its state key disappears from the room.
func TestConflictedStateIsKept(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()

		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		natsInstance := jetstream.NATSInstance{}
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat), test.RoomVersion(gomatrixserverlib.RoomVersionV12))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(bob.ID))
		powerLevels := room.CreateAndInsert(t, alice, spec.MRoomPowerLevels, map[string]any{
			"users": map[string]int{bob.ID: 9493420},
		}, test.WithStateKey(""))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))

		// Alice and bob change the join rules concurrently, and bob changes
		// the topic on his side of the fork.
		fork := test.WithPrevEvents(powerLevels.EventID())
		aliceRules := room.CreateEvent(t, alice, spec.MRoomJoinRules, gomatrixserverlib.JoinRuleContent{JoinRule: spec.Invite}, test.WithStateKey(""), fork)
		bobRules := room.CreateEvent(t, bob, spec.MRoomJoinRules, gomatrixserverlib.JoinRuleContent{JoinRule: spec.Knock}, test.WithStateKey(""), fork)
		aliceTopic := room.CreateEvent(t, alice, "m.room.topic", map[string]any{"topic": "alice"}, test.WithStateKey(""), test.WithPrevEvents(aliceRules.EventID()))
		bobTopic := room.CreateEvent(t, bob, "m.room.topic", map[string]any{"topic": "bob"}, test.WithStateKey(""), test.WithPrevEvents(bobRules.EventID()))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{aliceRules, aliceTopic, bobRules, bobTopic}, "test", "test", "test", nil, false))

		joinRules := api.GetStateEvent(ctx, rsAPI, room.ID, gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomJoinRules})
		require.NotNil(t, joinRules, "the join rules were dropped from the room state")
		// Control events apply from the highest power level to the lowest, so
		// the creator's goes first and bob's replaces it.
		require.Equal(t, spec.Knock, gjson.GetBytes(joinRules.Content(), "join_rule").Str)

		topic := api.GetStateEvent(ctx, rsAPI, room.ID, gomatrixserverlib.StateKeyTuple{EventType: "m.room.topic"})
		require.NotNil(t, topic, "the topic was dropped from the room state")
	})
}

// An outlier is only stored for its content, so it must not replace the forward
// extremity it points at. Otherwise the state that a remote server reports for
// a gap would replace the state of the room instead of being resolved with it.
func TestOutliersDoNotReferenceTheirPrevEvents(t *testing.T) {
	alice := test.NewUser(t)
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()

		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		db, err := storage.Open(ctx, cm, &cfg.RoomServer.Database, caches)
		require.NoError(t, err)

		room := test.NewRoom(t, alice)
		head := room.Events()[len(room.Events())-1]
		roomInfo, err := db.GetOrCreateRoomInfo(ctx, head.PDU)
		require.NoError(t, err)
		eventTypeNID, err := db.GetOrCreateEventTypeNID(ctx, "m.room.topic")
		require.NoError(t, err)
		stateKeyNID, err := db.GetOrCreateEventStateKeyNID(ctx, new(string))
		require.NoError(t, err)

		child := room.CreateEvent(t, alice, "m.room.topic", map[string]any{"topic": "child"}, test.WithStateKey(""), test.WithPrevEvents(head.EventID()))
		isReferenced := func() bool {
			updater, updaterErr := db.GetRoomUpdater(ctx, roomInfo)
			require.NoError(t, updaterErr)
			defer func() { require.NoError(t, updater.Rollback()) }()
			referenced, refErr := updater.IsReferenced(head.EventID())
			require.NoError(t, refErr)
			return referenced
		}

		_, _, err = db.StoreEvent(ctx, child.PDU, roomInfo, eventTypeNID, stateKeyNID, nil, false, true)
		require.NoError(t, err)
		require.False(t, isReferenced(), "an outlier replaced a forward extremity")

		// The same event arriving as a regular event is part of the DAG.
		_, _, err = db.StoreEvent(ctx, child.PDU, roomInfo, eventTypeNID, stateKeyNID, nil, false, false)
		require.NoError(t, err)
		require.True(t, isReferenced())
	})
}
