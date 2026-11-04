// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input_test

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	rsinternal "codefloe.com/pat-s/zendrite/roomserver/internal"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// An event citing a rejected event is rejected too, unless the cited event was
// only rejected because the prev events it arrived with could not be fetched.
func TestRejectedAuthEvents(t *testing.T) {
	alice := test.NewUser(t, test.WithSigningServer("test", "ed25519:test", test.PrivateKeyB))
	bob := test.NewUser(t, test.WithSigningServer(remoteServer, "ed25519:remote", test.PrivateKeyA))
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		join := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		known := len(room.Events())
		stateEvent := func(eventType, stateKey string) string {
			for _, ev := range room.Events() {
				if ev.Type() == eventType && ev.StateKeyEquals(stateKey) {
					return ev.EventID()
				}
			}
			t.Fatalf("no %s event for %q", eventType, stateKey)
			return ""
		}
		create := stateEvent(spec.MRoomCreate, "")
		powerLevels := stateEvent(spec.MRoomPowerLevels, "")
		aliceJoin := stateEvent(spec.MRoomMember, alice.ID)
		cites := func(eventID string) []string { return []string{create, join.EventID(), eventID} }

		// The test room refuses to build an event that fails auth. This grant
		// lets it, and is never sent, so the server sees Bob without power.
		room.CreateAndInsert(t, alice, spec.MRoomPowerLevels, map[string]any{"users": map[string]any{alice.ID: 100, bob.ID: 100}}, test.WithStateKey(""))

		// Bob may not change the power levels, so this fails its auth events.
		invalid := room.CreateEvent(t, bob, spec.MRoomPowerLevels, map[string]any{"users": map[string]any{alice.ID: 100, bob.ID: 100}},
			test.WithStateKey(""), test.WithPrevEvents(join.EventID()), test.WithAuthIDs(cites(powerLevels)))
		citesInvalid := room.CreateEvent(t, bob, "m.room.message", map[string]any{"body": "cites invalid"},
			test.WithPrevEvents(join.EventID()), test.WithAuthIDs(cites(invalid.EventID())))
		citesCitesInvalid := room.CreateEvent(t, bob, "m.room.message", map[string]any{"body": "cites the citing"},
			test.WithPrevEvents(join.EventID()), test.WithAuthIDs(cites(citesInvalid.EventID())))

		// A valid power level event, rejected as its prev event cannot be fetched.
		lost := room.CreateEvent(t, alice, "m.room.message", map[string]any{"body": "lost"},
			test.WithPrevEvents(join.EventID()), test.WithAuthIDs([]string{create, powerLevels, aliceJoin}))
		gapped := room.CreateEvent(t, alice, spec.MRoomPowerLevels, map[string]any{"users": map[string]any{alice.ID: 100, bob.ID: 100}, "users_default": 1},
			test.WithStateKey(""), test.WithPrevEvents(lost.EventID()), test.WithAuthIDs([]string{create, powerLevels, aliceJoin}))
		citesGapped := room.CreateEvent(t, bob, "m.room.message", map[string]any{"body": "cites gapped"},
			test.WithPrevEvents(join.EventID()), test.WithAuthIDs(cites(gapped.EventID())))

		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		keyRing := &gomatrixserverlib.KeyRing{KeyDatabase: serverKeys{"test": test.PrivateKeyB, remoteServer: test.PrivateKeyA}}
		federation := &remoteFederation{room: room, keyRing: keyRing}
		federation.failing.Store(true)
		rsAPI.SetFederationAPI(federation, keyRing)
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events()[:known], "test", "test", "test", nil, false))
		internalAPI, ok := rsAPI.(*rsinternal.RoomserverInternalAPI)
		require.True(t, ok)

		send := func(ev *types.HeaderedEvent) {
			err := api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{ev}, "test", remoteServer, api.DoNotSendToOtherServers, nil, false)
			if err != nil {
				require.True(t, api.IsInputRejected(err), "the event failed instead of being stored: %v", err)
			}
			internalAPI.ExpireMissingStateCooldowns()
		}
		rejected := func(ev *types.HeaderedEvent) bool {
			roomInfo, err := internalAPI.DB.RoomInfo(ctx, room.ID)
			require.NoError(t, err)
			isRejected, err := internalAPI.DB.IsEventRejected(ctx, roomInfo.RoomNID, ev.EventID())
			require.NoError(t, err)
			return isRejected
		}

		send(invalid)
		require.True(t, rejected(invalid))
		send(citesInvalid)
		require.True(t, rejected(citesInvalid), "an event citing a rejected event was accepted")
		send(citesCitesInvalid)
		require.True(t, rejected(citesCitesInvalid), "the rejection did not carry over a second event")
		// That a rejected event is left out of the auth events is not enough:
		// these events pass without it.

		send(gapped)
		require.True(t, rejected(gapped), "the event with a missing prev event was not rejected")
		send(citesGapped)
		require.False(t, rejected(citesGapped), "an event was rejected for citing one whose prev events are missing")
	})
}
