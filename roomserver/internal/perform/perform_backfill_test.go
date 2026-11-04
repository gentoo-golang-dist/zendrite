// Copyright 2025 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.
package perform

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"

	federationAPI "codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/storage"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/test"
)

// fakePurgedRoomDB simulates a room that has been purged while a federation
// backfill was in flight: RoomInfo reports the room no longer exists, but the
// legacy GetOrCreateRoomInfo path would happily re-create it.
type fakePurgedRoomDB struct {
	storage.Database
	getOrCreateCalled bool
	storeEventCalled  bool
}

func (f *fakePurgedRoomDB) EventNIDs(ctx context.Context, eventIDs []string) (map[string]types.EventMetadata, error) {
	return map[string]types.EventMetadata{}, nil
}

func (f *fakePurgedRoomDB) Events(ctx context.Context, roomVersion gomatrixserverlib.RoomVersion, eventNIDs []types.EventNID) ([]types.Event, error) {
	return nil, nil
}

func (f *fakePurgedRoomDB) RoomInfo(ctx context.Context, roomID string) (*types.RoomInfo, error) {
	// The room was purged, so it no longer exists.
	return nil, nil
}

func (f *fakePurgedRoomDB) GetOrCreateRoomInfo(ctx context.Context, event gomatrixserverlib.PDU) (*types.RoomInfo, error) {
	f.getOrCreateCalled = true
	return &types.RoomInfo{RoomNID: 1}, nil
}

func (f *fakePurgedRoomDB) GetOrCreateEventTypeNID(ctx context.Context, eventType string) (types.EventTypeNID, error) {
	return 1, nil
}

func (f *fakePurgedRoomDB) GetOrCreateEventStateKeyNID(ctx context.Context, eventStateKey *string) (types.EventStateKeyNID, error) {
	return 1, nil
}

func (f *fakePurgedRoomDB) StoreEvent(ctx context.Context, event gomatrixserverlib.PDU, roomInfo *types.RoomInfo, eventTypeNID types.EventTypeNID, eventStateKeyNID types.EventStateKeyNID, authEventNIDs []types.EventNID, isRejected, isOutlier bool) (types.EventNID, types.StateAtEvent, error) {
	f.storeEventCalled = true
	return 1, types.StateAtEvent{}, nil
}

func (f *fakePurgedRoomDB) MaybeRedactEvent(ctx context.Context, roomInfo *types.RoomInfo, eventNID types.EventNID, event gomatrixserverlib.PDU, plResolver state.PowerLevelResolver, querier api.QuerySenderIDAPI) (gomatrixserverlib.PDU, gomatrixserverlib.PDU, error) {
	return nil, nil, nil
}

// TestPersistEventsDoesNotRecreatePurgedRoom guards against issue #224: an
// in-flight backfill must not re-add a room that has been purged.
func TestPersistEventsDoesNotRecreatePurgedRoom(t *testing.T) {
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	events := room.Events()

	db := &fakePurgedRoomDB{}
	pdus := []gomatrixserverlib.PDU{events[len(events)-1].PDU}

	_, stored := persistEvents(context.Background(), db, pdus)

	if db.getOrCreateCalled {
		t.Error("persistEvents called GetOrCreateRoomInfo, which re-creates purged rooms; it must look the room up with RoomInfo instead")
	}
	if db.storeEventCalled {
		t.Error("persistEvents stored a backfilled event into a purged (non-existent) room, re-creating it")
	}
	if len(stored) != 0 {
		t.Errorf("expected no stored events for a purged room, got %d", len(stored))
	}
}

// stateServer counts the requests for the state of a room.
type stateServer struct {
	federationAPI.RoomserverFederationAPI
	events []gomatrixserverlib.PDU
	calls  int
}

func (s *stateServer) LookupState(
	_ context.Context, _, _ spec.ServerName, _, _ string, _ gomatrixserverlib.RoomVersion,
) (gomatrixserverlib.StateResponse, error) {
	s.calls++
	var res fclient.RespState
	for _, ev := range s.events {
		res.StateEvents = append(res.StateEvents, ev.JSON())
	}
	return &res, nil
}

// A state event that is not stored is fetched with the whole state of the
// room. The state at the next event holds it too, and the room's whole state
// must not be requested again for it.
func TestStateBeforeEventRemembersFetchedEvents(t *testing.T) {
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	events := gomatrixserverlib.ToPDUs(room.Events())
	eventIDs := make([]string, len(events))
	for i, ev := range events {
		eventIDs[i] = ev.EventID()
	}

	server := &stateServer{events: events}
	requester := newBackfillRequester(&fakePurgedRoomDB{}, server, nil, nil, "", "local", func(spec.ServerName) bool { return false },
		nil, nil, room.Version)
	requester.servers = []spec.ServerName{"remote"}

	for range 3 {
		res, err := requester.StateBeforeEvent(context.Background(), room.Version, events[len(events)-1], eventIDs)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != len(eventIDs) {
			t.Fatalf("expected %d events, got %d", len(eventIDs), len(res))
		}
	}
	if server.calls != 1 {
		t.Errorf("the state of the room was requested %d times, want once", server.calls)
	}
}

// The state before an event must not change when the state before the event
// after it is worked out.
func TestCalculateNewStateIDsKeepsPrevEventState(t *testing.T) {
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	events := room.Events()
	newName := room.CreateAndInsert(t, alice, "m.room.name", map[string]any{"name": "a"}, test.WithStateKey(""))
	newerName := room.CreateAndInsert(t, alice, "m.room.name", map[string]any{"name": "b"}, test.WithStateKey(""))
	next := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "hello"})

	requester := newBackfillRequester(&fakePurgedRoomDB{}, nil, nil, nil, "", "local", func(spec.ServerName) bool { return false },
		nil, nil, room.Version)
	before := make([]string, 0, len(events)+1)
	for _, ev := range events {
		requester.eventIDMap[ev.EventID()] = ev.PDU
		before = append(before, ev.EventID())
	}
	requester.eventIDMap[newName.EventID()] = newName.PDU
	before = append(before, newName.EventID())
	want := slices.Clone(before)
	requester.eventIDToBeforeStateIDs[newerName.EventID()] = before

	after := requester.calculateNewStateIDs(next.PDU, newerName.PDU, before)
	if !slices.Contains(after, newerName.EventID()) || slices.Contains(after, newName.EventID()) {
		t.Errorf("the state after the new name must hold it in place of the old one: %v", after)
	}
	if !slices.Equal(requester.eventIDToBeforeStateIDs[newerName.EventID()], want) {
		t.Errorf("the state before the new name changed: %v", requester.eventIDToBeforeStateIDs[newerName.EventID()])
	}
}

// eventServer answers requests for single events.
type eventServer struct {
	federationAPI.RoomserverFederationAPI
	pdu   gomatrixserverlib.PDU
	calls int
}

func (s *eventServer) GetEvent(
	_ context.Context, _, _ spec.ServerName, _ string,
) (gomatrixserverlib.Transaction, error) {
	s.calls++
	if s.pdu == nil {
		return gomatrixserverlib.Transaction{}, errors.New("not found")
	}
	return gomatrixserverlib.Transaction{PDUs: []json.RawMessage{s.pdu.JSON()}}, nil
}

// senderQuerier resolves senders without a room server.
type senderQuerier struct {
	api.QuerySenderIDAPI
}

func (senderQuerier) QueryUserIDForSender(_ context.Context, _ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
	return senderID.ToUserID(), nil
}

// failingVerifier refuses every signature.
type failingVerifier struct{}

func (failingVerifier) VerifyJSONs(
	_ context.Context, requests []gomatrixserverlib.VerifyJSONRequest,
) ([]gomatrixserverlib.VerifyJSONResult, error) {
	results := make([]gomatrixserverlib.VerifyJSONResult, len(requests))
	for i := range results {
		results[i].Error = errors.New("bad signature")
	}
	return results, nil
}

// An auth event decides whether other events are accepted, so what a server
// sends for it must be that event, and must be signed. An event that no server
// can provide is only asked for once.
func TestFetchAuthEvent(t *testing.T) {
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
	other := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
	event := room.Events()[0]

	for name, tc := range map[string]struct {
		roomID  string
		wantID  string
		sent    gomatrixserverlib.PDU
		keyRing gomatrixserverlib.JSONVerifier
		valid   bool
	}{
		"valid":         {roomID: room.ID, wantID: event.EventID(), sent: event, keyRing: &test.NopJSONVerifier{}, valid: true},
		"bad signature": {roomID: room.ID, wantID: event.EventID(), sent: event, keyRing: failingVerifier{}},
		"other event":   {roomID: room.ID, wantID: event.EventID(), sent: room.Events()[1], keyRing: &test.NopJSONVerifier{}},
		"other room":    {roomID: other.ID, wantID: event.EventID(), sent: event, keyRing: &test.NopJSONVerifier{}},
		"not found":     {roomID: room.ID, wantID: event.EventID(), keyRing: &test.NopJSONVerifier{}},
	} {
		t.Run(name, func(t *testing.T) {
			server := &eventServer{pdu: tc.sent}
			requester := newBackfillRequester(&fakePurgedRoomDB{}, server, senderQuerier{}, tc.keyRing, tc.roomID, "local",
				func(spec.ServerName) bool { return false }, nil, nil, room.Version)
			requester.servers = []spec.ServerName{"remote"}

			for range 2 {
				got := requester.fetchAuthEvent(tc.wantID)
				if tc.valid && (got == nil || got.EventID() != tc.wantID) {
					t.Fatalf("expected the event, got %v", got)
				}
				if !tc.valid && got != nil {
					t.Fatalf("expected no event, got %s", got.EventID())
				}
			}
			if server.calls != 1 {
				t.Errorf("the event was requested %d times, want once", server.calls)
			}
		})
	}
}

// Only auth events of accepted events are kept, each before those that need it.
func TestFetchedAuthEventsOf(t *testing.T) {
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
	events := gomatrixserverlib.ToPDUs(room.Events())
	create, member := events[0], events[1]
	message := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "hello"})
	unrelated := room.Events()[2]

	requester := newBackfillRequester(&fakePurgedRoomDB{}, nil, nil, nil, room.ID, "local",
		func(spec.ServerName) bool { return false }, nil, nil, room.Version)
	requester.fetchedAuthEvents = map[string]gomatrixserverlib.PDU{
		create.EventID():               create,
		member.EventID():               member,
		"$no-server-had-this-one":      nil,
		"$fetched-for-a-refused-event": unrelated,
	}

	got := requester.fetchedAuthEventsOf([]gomatrixserverlib.PDU{message})
	if len(got) != 2 || got[0].EventID() != create.EventID() || got[1].EventID() != member.EventID() {
		ids := make([]string, len(got))
		for i, ev := range got {
			ids[i] = ev.EventID()
		}
		t.Fatalf("expected the create event then the membership, got %v", ids)
	}
}
