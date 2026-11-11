// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fsapi "codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

const partialRemote = spec.ServerName("remote.test")

// partialJoin is a room on a remote server that a local user joins with
// partial state (MSC3706): the remote server sends the state before the join
// without memberships.
type partialJoin struct {
	t        *testing.T
	rsAPI    api.RoomserverInternalAPI
	room     *test.Room
	roomNID  types.RoomNID
	join     *types.HeaderedEvent
	prevJoin *types.HeaderedEvent
	// stateBeforePrev is the full state before the join's prev event.
	stateBeforePrev []*types.HeaderedEvent
}

func newPartialJoinRS(t *testing.T, dbType test.DBType) api.RoomserverInternalAPI {
	t.Helper()
	cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
	t.Cleanup(closeDB)
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	natsInstance := jetstream.NATSInstance{}
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(&noFederation{}, nil)
	return rsAPI
}

// noFederation knows no other servers: every event the roomserver needs is
// given to it.
type noFederation struct {
	fsapi.RoomserverFederationAPI
}

func (f *noFederation) QueryJoinedHostServerNamesInRoom(
	context.Context, *fsapi.QueryJoinedHostServerNamesInRoomRequest, *fsapi.QueryJoinedHostServerNamesInRoomResponse,
) error {
	return nil
}

func (f *noFederation) IsServerBackingOff(spec.ServerName) bool { return false }

func (f *noFederation) GetEventAuth(
	context.Context, spec.ServerName, spec.ServerName, gomatrixserverlib.RoomVersion, string, string,
) (fclient.RespEventAuth, error) {
	return fclient.RespEventAuth{}, errors.New("no federation in this test")
}

// beginPartialJoin joins alice to room, which already holds every event
// before the join, with partial state. StateBeforePrev is the full state
// before the last event of the room, the join's prev event.
func beginPartialJoin(
	t *testing.T, rsAPI api.RoomserverInternalAPI, room *test.Room, alice *test.User, stateBeforePrev []*types.HeaderedEvent,
) *partialJoin {
	t.Helper()
	ctx := context.Background()
	events := room.Events()
	pj := &partialJoin{t: t, rsAPI: rsAPI, room: room, prevJoin: events[len(events)-1], stateBeforePrev: stateBeforePrev}

	// The partial state: the state before the join without memberships, and
	// its auth chain.
	var partial, authChain []*types.HeaderedEvent
	for _, ev := range room.CurrentState() {
		if ev.Type() != spec.MRoomMember {
			partial = append(partial, ev)
		}
	}
	for _, ev := range events {
		if ev.StateKey() != nil && ev.EventID() != pj.prevJoin.EventID() {
			authChain = append(authChain, ev)
		}
	}
	pj.join = room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(alice.ID))

	roomID, err := spec.NewRoomID(room.ID)
	require.NoError(t, err)
	pj.roomNID, err = rsAPI.AssignRoomNID(ctx, *roomID, room.Version)
	require.NoError(t, err)
	require.NoError(t, rsAPI.SetRoomPartialState(ctx, pj.roomNID, 0, string(partialRemote), []string{string(partialRemote)}, 0))
	require.NoError(t, api.SendEventWithState(ctx, rsAPI, "test", api.KindNew, &fclient.RespState{
		StateEvents: types.NewEventJSONsFromHeaderedEvents(partial),
		AuthEvents:  types.NewEventJSONsFromHeaderedEvents(authChain),
	}, pj.join, partialRemote, nil, false))
	return pj
}

// resync runs the partial-state resync to its end, answering requests for the
// state before the join's prev event with the full state.
func (pj *partialJoin) resync() {
	t := pj.t
	ctx := context.Background()
	for range 100 {
		batch, err := pj.rsAPI.PartialStateResyncBatch(ctx, pj.room.ID, 10)
		require.NoError(t, err)
		if len(batch) == 0 {
			cleared, cerr := pj.rsAPI.CompletePartialStateResync(ctx, pj.room.ID)
			require.NoError(t, cerr)
			require.True(t, cleared)
			return
		}
		for _, eventID := range batch {
			pj.resyncEvent(eventID)
		}
	}
	t.Fatal("the resync did not end")
}

func (pj *partialJoin) resyncEvent(eventID string) {
	t := pj.t
	ctx := context.Background()
	need, err := pj.rsAPI.ResyncPartialStateEvent(ctx, pj.room.ID, eventID, nil)
	require.NoError(t, err)
	if len(need) == 0 {
		return
	}
	require.Equal(t, []string{pj.prevJoin.EventID()}, need)
	// The resync fetches the prev event and the state before it.
	outliers := append([]*types.HeaderedEvent{}, pj.stateBeforePrev...)
	outliers = append(outliers, pj.prevJoin)
	var inputs []api.InputRoomEvent
	for _, ev := range gomatrixserverlib.ReverseTopologicalOrdering(toPDUs(outliers), gomatrixserverlib.TopologicalOrderByAuthEvents) {
		inputs = append(inputs, api.InputRoomEvent{Kind: api.KindOutlier, Event: &types.HeaderedEvent{PDU: ev}, Origin: partialRemote})
	}
	var res api.InputRoomEventsResponse
	pj.rsAPI.InputRoomEvents(ctx, &api.InputRoomEventsRequest{InputRoomEvents: inputs}, &res)
	require.Empty(t, res.ErrMsg)
	ids := make([]string, 0, len(pj.stateBeforePrev))
	for _, ev := range pj.stateBeforePrev {
		ids = append(ids, ev.EventID())
	}
	need, err = pj.rsAPI.ResyncPartialStateEvent(ctx, pj.room.ID, eventID, map[string][]string{pj.prevJoin.EventID(): ids})
	require.NoError(t, err)
	require.Empty(t, need)
}

func toPDUs(events []*types.HeaderedEvent) []gomatrixserverlib.PDU {
	pdus := make([]gomatrixserverlib.PDU, len(events))
	for i := range events {
		pdus[i] = events[i].PDU
	}
	return pdus
}

func currentMembers(t *testing.T, rsAPI api.RoomserverInternalAPI, roomID string) map[string]string {
	t.Helper()
	var res api.QueryLatestEventsAndStateResponse
	require.NoError(t, rsAPI.QueryLatestEventsAndState(context.Background(), &api.QueryLatestEventsAndStateRequest{RoomID: roomID}, &res))
	members := map[string]string{}
	for _, ev := range res.StateEvents {
		if ev.Type() == spec.MRoomMember {
			membership, _ := ev.Membership()
			members[*ev.StateKey()] = membership
		}
	}
	return members
}

func TestPartialStateJoinResync(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		rsAPI := newPartialJoinRS(t, dbType)
		charlie := test.NewUser(t, test.WithSigningServer(partialRemote, "ed25519:remote", test.PrivateKeyB))
		derek := test.NewUser(t, test.WithSigningServer(partialRemote, "ed25519:remote", test.PrivateKeyB))
		alice := test.NewUser(t)
		room := test.NewRoom(t, charlie, test.RoomPreset(test.PresetPublicChat))
		before := room.CurrentState()
		room.CreateAndInsert(t, derek, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(derek.ID))

		pj := beginPartialJoin(t, rsAPI, room, alice, before)
		partial, err := rsAPI.IsRoomPartialState(ctx, pj.roomNID)
		require.NoError(t, err)
		require.True(t, partial)
		// Only alice is known to be in the room.
		assert.Equal(t, map[string]string{alice.ID: spec.Join}, currentMembers(t, rsAPI, room.ID))

		// A local message during the resync has partial state too.
		msg := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "hi"})
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{msg}, "test", "test", "test", nil, false))

		// The join goes first, as the message waits for it.
		batch, err := rsAPI.PartialStateResyncBatch(ctx, room.ID, 10)
		require.NoError(t, err)
		assert.Equal(t, []string{pj.join.EventID()}, batch)
		// Nothing completes while events have partial state.
		cleared, err := rsAPI.CompletePartialStateResync(ctx, room.ID)
		require.NoError(t, err)
		assert.False(t, cleared)

		pj.resync()

		partial, err = rsAPI.IsRoomPartialState(ctx, pj.roomNID)
		require.NoError(t, err)
		assert.False(t, partial)
		assert.Equal(t, map[string]string{
			alice.ID: spec.Join, charlie.ID: spec.Join, derek.ID: spec.Join,
		}, currentMembers(t, rsAPI, room.ID))
	})
}

// TestPartialStateJoinRemoteMembershipWaits checks that building an event
// whose auth needs the membership of a remote user, which the partial state
// lacks, waits for the full state, while other events do not.
func TestPartialStateJoinRemoteMembershipWaits(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		rsAPI := newPartialJoinRS(t, dbType)
		charlie := test.NewUser(t, test.WithSigningServer(partialRemote, "ed25519:remote", test.PrivateKeyB))
		alice := test.NewUser(t)
		room := test.NewRoom(t, charlie, test.RoomPreset(test.PresetPublicChat))
		pj := beginPartialJoin(t, rsAPI, room, alice, room.CurrentState())

		query := func(ctx context.Context, stateKey string) error {
			res := api.QueryLatestEventsAndStateResponse{}
			return rsAPI.QueryLatestEventsAndState(ctx, &api.QueryLatestEventsAndStateRequest{
				RoomID: room.ID,
				StateToFetch: []gomatrixserverlib.StateKeyTuple{
					{EventType: spec.MRoomPowerLevels},
					{EventType: spec.MRoomMember, StateKey: stateKey},
				},
			}, &res)
		}
		// A local user's membership is in the partial state.
		require.NoError(t, query(ctx, alice.ID))

		// Charlie's is not, so kicking him waits for the full state.
		waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		require.ErrorIs(t, query(waitCtx, charlie.ID), context.DeadlineExceeded)

		done := make(chan error, 1)
		go func() { done <- query(ctx, charlie.ID) }()
		pj.resync()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the query did not return once the room had its full state")
		}
	})
}

// remoteEvent builds an event of a remote user with the given prev and auth
// events, as a remote server could send it.
func remoteEvent(
	t *testing.T, room *test.Room, sender *test.User, evType string, stateKey *string, content map[string]any,
	prevs, auth []*types.HeaderedEvent,
) *types.HeaderedEvent {
	t.Helper()
	ids := func(evs []*types.HeaderedEvent) []string {
		out := make([]string, 0, len(evs))
		for _, ev := range evs {
			out = append(out, ev.EventID())
		}
		return out
	}
	var depth int64
	for _, prev := range prevs {
		depth = max(depth, prev.Depth())
	}
	proto := gomatrixserverlib.ProtoEvent{
		SenderID: sender.ID, RoomID: room.ID, Type: evType, StateKey: stateKey,
		PrevEvents: ids(prevs), AuthEvents: ids(auth), Depth: depth + 1,
	}
	require.NoError(t, proto.SetContent(content))
	ev, err := gomatrixserverlib.MustGetRoomVersion(room.Version).NewEventBuilderFromProtoEvent(&proto).
		Build(time.Now(), partialRemote, "ed25519:remote", test.PrivateKeyB)
	require.NoError(t, err)
	return &types.HeaderedEvent{PDU: ev}
}

func isRejected(t *testing.T, rsAPI api.RoomserverInternalAPI, eventID string) bool {
	t.Helper()
	res := api.QueryEventsByIDResponse{}
	require.NoError(t, rsAPI.QueryEventsByID(context.Background(), &api.QueryEventsByIDRequest{
		EventIDs: []string{eventID}, ExcludeRejected: true,
	}, &res))
	return len(res.Events) == 0
}

// TestPartialStateJoinRecheck checks that the resync checks the events
// received during the partial state against the full state: events the
// partial state let through are rejected when the full state does not, as in
// Complement's "State accepted incorrectly", and the other way round.
func TestPartialStateJoinRecheck(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		rsAPI := newPartialJoinRS(t, dbType)
		charlie := test.NewUser(t, test.WithSigningServer(partialRemote, "ed25519:remote", test.PrivateKeyB))
		derek := test.NewUser(t, test.WithSigningServer(partialRemote, "ed25519:remote", test.PrivateKeyB))
		elsie := test.NewUser(t, test.WithSigningServer(partialRemote, "ed25519:remote", test.PrivateKeyB))
		alice := test.NewUser(t)
		room := test.NewRoom(t, charlie, test.RoomPreset(test.PresetPublicChat))
		var create, pl *types.HeaderedEvent
		for _, ev := range room.Events() {
			if ev.Type() == spec.MRoomCreate {
				create = ev
			}
		}
		pl = room.CreateAndInsert(t, charlie, spec.MRoomPowerLevels, map[string]any{
			"users": map[string]any{charlie.ID: 100, derek.ID: 100, elsie.ID: 50}, "state_default": 50, "kick": 50,
		}, test.WithStateKey(""))
		derekJoin := room.CreateAndInsert(t, derek, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(derek.ID))
		elsieJoin := room.CreateAndInsert(t, elsie, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(elsie.ID))
		before := room.CurrentState()
		room.CreateAndInsert(t, derek, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(derek.ID))

		pj := beginPartialJoin(t, rsAPI, room, alice, before)

		// derek, who left, kicks elsie, citing his join: the partial state
		// does not know he left.
		elsieKey := elsie.ID
		badKick := remoteEvent(t, room, derek, spec.MRoomMember, &elsieKey, map[string]any{"membership": spec.Leave},
			[]*types.HeaderedEvent{pj.join}, []*types.HeaderedEvent{create, pl, derekJoin, elsieJoin})
		// elsie sends state: against the partial state, she was kicked.
		empty := ""
		elsieState := remoteEvent(t, room, elsie, "m.room.test", &empty, map[string]any{"body": "elsie"},
			[]*types.HeaderedEvent{badKick}, []*types.HeaderedEvent{create, pl, elsieJoin})
		// The resident server hands out the auth events this server lacks.
		var res api.InputRoomEventsResponse
		rsAPI.InputRoomEvents(ctx, &api.InputRoomEventsRequest{InputRoomEvents: []api.InputRoomEvent{
			{Kind: api.KindOutlier, Event: derekJoin, Origin: partialRemote},
			{Kind: api.KindOutlier, Event: elsieJoin, Origin: partialRemote},
		}}, &res)
		require.Empty(t, res.ErrMsg)
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{badKick}, "test", partialRemote, partialRemote, nil, false))
		err := api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{elsieState}, "test", partialRemote, partialRemote, nil, false)
		require.True(t, err == nil || strings.Contains(err.Error(), api.InputWasRejected), "unexpected error: %v", err)

		assert.False(t, isRejected(t, rsAPI, badKick.EventID()), "the kick is accepted against the partial state")
		assert.True(t, isRejected(t, rsAPI, elsieState.EventID()), "elsie's event is rejected against the partial state")

		pj.resync()

		assert.True(t, isRejected(t, rsAPI, badKick.EventID()), "the kick is rejected against the full state")
		assert.False(t, isRejected(t, rsAPI, elsieState.EventID()), "elsie's event is accepted against the full state")
		members := currentMembers(t, rsAPI, room.ID)
		assert.Equal(t, spec.Join, members[elsie.ID])
		assert.Equal(t, spec.Leave, members[derek.ID])
		assert.Equal(t, spec.Join, members[alice.ID])
	})
}

// TestPartialStateJoinResume checks that a resync that stops after some events
// resumes with the events that are left, and that a second local join while
// the room has partial state keeps the first join in the room's state.
func TestPartialStateJoinResume(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		rsAPI := newPartialJoinRS(t, dbType)
		charlie := test.NewUser(t, test.WithSigningServer(partialRemote, "ed25519:remote", test.PrivateKeyB))
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		room := test.NewRoom(t, charlie, test.RoomPreset(test.PresetPublicChat))
		before := room.CurrentState()
		room.CreateAndInsert(t, charlie, "m.room.message", map[string]any{"body": "hello"})

		pj := beginPartialJoin(t, rsAPI, room, alice, before)

		// bob joins too, through the remote server, which again returns
		// partial state without alice's membership.
		var partial []*types.HeaderedEvent
		for _, ev := range before {
			if ev.Type() != spec.MRoomMember {
				partial = append(partial, ev)
			}
		}
		bobJoin := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		require.NoError(t, rsAPI.SetRoomPartialState(ctx, pj.roomNID, 0, string(partialRemote), []string{string(partialRemote)}, 0))
		require.NoError(t, api.SendEventWithState(ctx, rsAPI, "test", api.KindNew, &fclient.RespState{
			StateEvents: types.NewEventJSONsFromHeaderedEvents(partial),
		}, bobJoin, partialRemote, nil, false))
		assert.Equal(t, map[string]string{alice.ID: spec.Join, bob.ID: spec.Join}, currentMembers(t, rsAPI, room.ID))

		// Only alice's join is resynced, as if the server stopped then.
		pj.resyncEvent(pj.join.EventID())
		partialNow, err := rsAPI.IsRoomPartialState(ctx, pj.roomNID)
		require.NoError(t, err)
		assert.True(t, partialNow)
		batch, err := rsAPI.PartialStateResyncBatch(ctx, room.ID, 10)
		require.NoError(t, err)
		assert.Equal(t, []string{bobJoin.EventID()}, batch, "the resync resumes with bob's join")

		pj.resync()
		assert.Equal(t, map[string]string{
			alice.ID: spec.Join, bob.ID: spec.Join, charlie.ID: spec.Join,
		}, currentMembers(t, rsAPI, room.ID))
	})
}

// TestPartialStateJoinRemoteLeave checks that a leave received during the
// partial state, of a member the partial state does not show, is in the
// current state after the resync.
func TestPartialStateJoinRemoteLeave(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		rsAPI := newPartialJoinRS(t, dbType)
		charlie := test.NewUser(t, test.WithSigningServer(partialRemote, "ed25519:remote", test.PrivateKeyB))
		elsie := test.NewUser(t, test.WithSigningServer("elsie.test", "ed25519:elsie", test.PrivateKeyB))
		alice := test.NewUser(t)
		room := test.NewRoom(t, charlie, test.RoomPreset(test.PresetPublicChat))
		before := room.CurrentState()
		elsieJoin := room.CreateAndInsert(t, elsie, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(elsie.ID))
		pj := beginPartialJoin(t, rsAPI, room, alice, before)

		var create, pl, jr *types.HeaderedEvent
		for _, ev := range room.Events() {
			switch ev.Type() {
			case spec.MRoomCreate:
				create = ev
			case spec.MRoomPowerLevels:
				pl = ev
			case spec.MRoomJoinRules:
				jr = ev
			}
		}
		_ = jr
		elsieKey := elsie.ID
		leave := remoteEvent(t, room, elsie, spec.MRoomMember, &elsieKey, map[string]any{"membership": spec.Leave},
			[]*types.HeaderedEvent{pj.join}, []*types.HeaderedEvent{create, pl, elsieJoin})
		var res api.InputRoomEventsResponse
		rsAPI.InputRoomEvents(ctx, &api.InputRoomEventsRequest{InputRoomEvents: []api.InputRoomEvent{
			{Kind: api.KindOutlier, Event: elsieJoin, Origin: partialRemote},
		}}, &res)
		require.Empty(t, res.ErrMsg)
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{leave}, "test", partialRemote, partialRemote, nil, false))
		assert.Equal(t, spec.Leave, currentMembers(t, rsAPI, room.ID)[elsie.ID])

		pj.resync()
		members := currentMembers(t, rsAPI, room.ID)
		assert.Equal(t, spec.Leave, members[elsie.ID])
		assert.Equal(t, spec.Join, members[alice.ID])
		assert.Equal(t, spec.Join, members[charlie.ID])

		// The state after later events builds on the resynced state: the
		// leave, whose state was stored again, stays in it.
		msg := remoteEvent(t, room, charlie, "m.room.message", nil, map[string]any{"body": "after"},
			[]*types.HeaderedEvent{leave}, []*types.HeaderedEvent{create, pl, room.Events()[1]})
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{msg}, "test", partialRemote, partialRemote, nil, false))
		assert.Equal(t, spec.Leave, currentMembers(t, rsAPI, room.ID)[elsie.ID])
	})
}
