// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	fsAPI "codefloe.com/pat-s/zendrite/federationapi/api"
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

// eventAuthFederationAPI answers /event_auth with a fixed auth chain, and
// /get_missing_events with its last event.
type eventAuthFederationAPI struct {
	fsAPI.RoomserverFederationAPI
	chain   []*types.HeaderedEvent
	keyRing *gomatrixserverlib.KeyRing
}

func (f eventAuthFederationAPI) KeyRing() *gomatrixserverlib.KeyRing { return f.keyRing }

func (eventAuthFederationAPI) QueryJoinedHostServerNamesInRoom(
	context.Context, *fsAPI.QueryJoinedHostServerNamesInRoomRequest, *fsAPI.QueryJoinedHostServerNamesInRoomResponse,
) error {
	return nil
}

func (eventAuthFederationAPI) IsServerBackingOff(spec.ServerName) bool { return false }

// LookupMissingEvents serves the last event of the chain, which the event
// being processed also has as its prev event.
func (f eventAuthFederationAPI) LookupMissingEvents(
	context.Context, spec.ServerName, spec.ServerName, string, fclient.MissingEvents, gomatrixserverlib.RoomVersion,
) (fclient.RespMissingEvents, error) {
	return fclient.RespMissingEvents{Events: gomatrixserverlib.EventJSONs{f.chain[len(f.chain)-1].JSON()}}, nil
}

func (f eventAuthFederationAPI) GetEventAuth(
	context.Context, spec.ServerName, spec.ServerName, gomatrixserverlib.RoomVersion, string, string,
) (fclient.RespEventAuth, error) {
	var res fclient.RespEventAuth
	for _, ev := range f.chain {
		res.AuthEvents = append(res.AuthEvents, ev.JSON())
	}
	return res, nil
}

// An auth event fetched over federation must be stored under its own state
// key. It used to get the state key of the event it authorizes, so a
// membership fetched for a message, which has no state key, was stored as if
// it were not state at all.
func TestFetchedAuthEventKeepsItsStateKey(t *testing.T) {
	alice := test.NewUser(t, test.WithSigningServer(remoteServer, "ed25519:remote", test.PrivateKeyA))
	bob := test.NewUser(t, test.WithSigningServer(remoteServer, "ed25519:remote", test.PrivateKeyA))
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		known := room.Events()
		// Bob's display name change never reached this server, but his next
		// message cites it as an auth event.
		rename := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join, "displayname": "Bob"}, test.WithStateKey(bob.ID))
		message := room.CreateEvent(t, bob, "m.room.message", map[string]any{"body": "hello"})
		require.Contains(t, message.AuthEventIDs(), rename.EventID())

		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		public, ok := test.PrivateKeyA.Public().(ed25519.PublicKey)
		require.True(t, ok)
		keyRing := &gomatrixserverlib.KeyRing{KeyDatabase: remoteKeys{public: public}}
		rsAPI.SetFederationAPI(eventAuthFederationAPI{chain: append(known, rename), keyRing: keyRing}, keyRing)
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, known, "test", "test", "test", nil, false))

		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{message}, "test", remoteServer, "test", nil, false))

		internalAPI, ok := rsAPI.(*rsinternal.RoomserverInternalAPI)
		require.True(t, ok)
		entries, err := internalAPI.DB.StateEntriesForEventIDs(ctx, []string{rename.EventID()}, false)
		require.NoError(t, err)
		require.Len(t, entries, 1, "the fetched auth event was not stored")
		keys, err := internalAPI.DB.EventStateKeyNIDs(ctx, []string{bob.ID})
		require.NoError(t, err)
		require.NotZero(t, keys[bob.ID])
		require.Equal(t, keys[bob.ID], entries[0].EventStateKeyNID, "the fetched membership was stored under another state key")
	})
}

// gapFederationAPI fails /event_auth, as a server that does not serve it
// does, and answers /get_missing_events with the given events.
type gapFederationAPI struct {
	eventAuthFederationAPI
	missing []*types.HeaderedEvent
	// authChain, if set, is served by /event_auth instead of failing.
	authChain []*types.HeaderedEvent
}

func (f gapFederationAPI) LookupMissingEvents(
	context.Context, spec.ServerName, spec.ServerName, string, fclient.MissingEvents, gomatrixserverlib.RoomVersion,
) (fclient.RespMissingEvents, error) {
	var res fclient.RespMissingEvents
	for _, ev := range f.missing {
		res.Events = append(res.Events, ev.JSON())
	}
	return res, nil
}

func (f gapFederationAPI) GetEventAuth(
	context.Context, spec.ServerName, spec.ServerName, gomatrixserverlib.RoomVersion, string, string,
) (fclient.RespEventAuth, error) {
	if f.authChain == nil {
		return fclient.RespEventAuth{}, &fsAPI.FederationClientError{Code: 404}
	}
	var res fclient.RespEventAuth
	for _, ev := range f.authChain {
		res.AuthEvents = append(res.AuthEvents, ev.JSON())
	}
	return res, nil
}

func newAuthTestRoomserver(t *testing.T, dbType test.DBType, federation fsAPI.RoomserverFederationAPI) (api.RoomserverInternalAPI, *rsinternal.RoomserverInternalAPI) {
	t.Helper()
	cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
	t.Cleanup(closeDB)
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	natsInstance := jetstream.NATSInstance{}
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	public, ok := test.PrivateKeyA.Public().(ed25519.PublicKey)
	require.True(t, ok)
	keyRing := &gomatrixserverlib.KeyRing{KeyDatabase: remoteKeys{public: public}}
	if f, ok := federation.(gapFederationAPI); ok {
		f.keyRing = keyRing
		federation = f
	}
	rsAPI.SetFederationAPI(federation, keyRing)
	internalAPI, ok := rsAPI.(*rsinternal.RoomserverInternalAPI)
	require.True(t, ok)
	return rsAPI, internalAPI
}

// requireAccepted fails unless the event is stored and was not rejected.
func requireAccepted(ctx context.Context, t *testing.T, internalAPI *rsinternal.RoomserverInternalAPI, roomID, eventID string) {
	t.Helper()
	info, err := internalAPI.DB.RoomInfo(ctx, roomID)
	require.NoError(t, err)
	require.NotNil(t, info)
	events, err := internalAPI.DB.EventsFromIDs(ctx, info, []string{eventID})
	require.NoError(t, err)
	require.Len(t, events, 1, "the event was not stored")
	rejected, err := internalAPI.DB.IsEventRejected(ctx, info.RoomNID, eventID)
	require.NoError(t, err)
	require.False(t, rejected, "the event was rejected")
}

// An event whose prev events are missing can cite an auth event that only
// the missing events bring in. When /event_auth does not provide it, the
// event used to be dropped before /get_missing_events was even tried.
func TestAuthEventsAreFetchedAgainAfterMissingPrevEvents(t *testing.T) {
	alice := test.NewUser(t, test.WithSigningServer(remoteServer, "ed25519:remote", test.PrivateKeyA))
	bob := test.NewUser(t, test.WithSigningServer(remoteServer, "ed25519:remote", test.PrivateKeyA))
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		known := room.Events()
		// Bob's rename never reached this server; his message follows it
		// and cites it as an auth event.
		rename := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join, "displayname": "Bob"}, test.WithStateKey(bob.ID))
		message := room.CreateEvent(t, bob, "m.room.message", map[string]any{"body": "hello"})
		require.Contains(t, message.AuthEventIDs(), rename.EventID())
		require.Contains(t, message.PrevEventIDs(), rename.EventID())

		rsAPI, internalAPI := newAuthTestRoomserver(t, dbType, gapFederationAPI{missing: []*types.HeaderedEvent{rename}})
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, known, "test", "test", "test", nil, false))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{message}, "test", remoteServer, "test", nil, false))

		requireAccepted(ctx, t, internalAPI, room.ID, rename.EventID())
		requireAccepted(ctx, t, internalAPI, room.ID, message.EventID())
	})
}

// A server may leave an auth event's own auth events out of /event_auth
// when this server already has them. They used to be looked up only among
// the auth events of the event being processed, which failed it.
func TestAuthEventOfFetchedAuthEventIsFoundLocally(t *testing.T) {
	alice := test.NewUser(t, test.WithSigningServer(remoteServer, "ed25519:remote", test.PrivateKeyA))
	bob := test.NewUser(t, test.WithSigningServer(remoteServer, "ed25519:remote", test.PrivateKeyA))
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		first := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join, "displayname": "B"}, test.WithStateKey(bob.ID))
		known := room.Events()
		second := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join, "displayname": "Bob"}, test.WithStateKey(bob.ID))
		require.Contains(t, second.AuthEventIDs(), first.EventID())
		// The message follows the first rename, which this server has, but
		// cites the second as Bob's membership.
		message := room.CreateEvent(t, bob, "m.room.message", map[string]any{"body": "hello"}, test.WithPrevEvents(first.EventID()))
		require.Contains(t, message.AuthEventIDs(), second.EventID())
		require.NotContains(t, message.AuthEventIDs(), first.EventID())

		// The auth chain leaves out the first rename.
		var chain []*types.HeaderedEvent
		for _, ev := range known {
			if ev.EventID() != first.EventID() {
				chain = append(chain, ev)
			}
		}
		chain = append(chain, second)
		rsAPI, internalAPI := newAuthTestRoomserver(t, dbType, gapFederationAPI{authChain: chain})
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, known, "test", "test", "test", nil, false))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{message}, "test", remoteServer, "test", nil, false))

		requireAccepted(ctx, t, internalAPI, room.ID, second.EventID())
		requireAccepted(ctx, t, internalAPI, room.ID, message.EventID())
	})
}
