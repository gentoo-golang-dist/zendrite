package query

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	fs "codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

type hierarchyFederation struct {
	fs.RoomserverFederationAPI
	err error
	// errs overrides err per server.
	errs map[spec.ServerName]error
	// answerFor, if set, is the room ID the server answers for.
	answerFor string
	calls     int
}

func (f *hierarchyFederation) RoomHierarchies(
	_ context.Context, _, dst spec.ServerName, roomID string, _ bool,
) (fclient.RoomHierarchyResponse, error) {
	f.calls++
	if err, ok := f.errs[dst]; ok && err != nil {
		return fclient.RoomHierarchyResponse{}, err
	}
	if f.err != nil {
		return fclient.RoomHierarchyResponse{}, f.err
	}
	if f.answerFor != "" {
		roomID = f.answerFor
	}
	return fclient.RoomHierarchyResponse{Room: fclient.RoomHierarchyRoom{PublicRoom: fclient.PublicRoom{RoomID: roomID}}}, nil
}

func TestFederatedRoomInfoNegativeCache(t *testing.T) {
	cfg := &config.Zendrite{}
	cfg.Global.ServerName = "local"
	caller := types.NewDeviceNotServerName(userapi.Device{UserID: "@alice:local"})
	roomID, err := spec.NewRoomID("!room:remote")
	require.NoError(t, err)
	newQuerier := func(federation *hierarchyFederation) *Queryer {
		return &Queryer{Cfg: cfg, FSAPI: federation, Cache: caching.NewRistrettoCache(64<<20, time.Hour, caching.DisableMetrics)}
	}
	// The cache stores entries asynchronously.
	eventuallyCached := func(t *testing.T, querier *Queryer, key string) {
		t.Helper()
		require.Eventually(t, func() bool { return querier.Cache.GetRoomHierarchyFailure(key) }, time.Second, time.Millisecond)
	}

	t.Run("a refusal is cached apart from the room", func(t *testing.T) {
		federation := &hierarchyFederation{err: &fs.FederationClientError{Code: 404}}
		querier := newQuerier(federation)
		require.Nil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"}, true))
		eventuallyCached(t, querier, roomHierarchyRefusalKey(t.Context(), querier, *roomID, nil, []string{"remote"}))
		require.False(t, querier.Cache.GetRoomHierarchyFailure(roomID.String()))
		require.Nil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"}, true))
		require.Equal(t, 1, federation.calls)
	})
	t.Run("an unreachable server is cached for the room", func(t *testing.T) {
		federation := &hierarchyFederation{err: &fs.FederationClientError{Code: 502}}
		querier := newQuerier(federation)
		require.Nil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"}, true))
		eventuallyCached(t, querier, roomID.String())
		federation.err = nil
		require.Nil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"}, true))
		require.Equal(t, 1, federation.calls)
	})
	t.Run("a refusal does not hide an unreachable server", func(t *testing.T) {
		federation := &hierarchyFederation{errs: map[spec.ServerName]error{
			"refusing": &fs.FederationClientError{Code: 404}, "down": &fs.FederationClientError{Code: 502},
		}}
		querier := newQuerier(federation)
		require.Nil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"refusing", "down"}, true))
		eventuallyCached(t, querier, roomID.String())
	})
	t.Run("an answer is returned", func(t *testing.T) {
		querier := newQuerier(&hierarchyFederation{})
		res := federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"}, true)
		require.NotNil(t, res)
		require.Equal(t, roomID.String(), res.Room.RoomID)
	})
	t.Run("an answer for another room is refused", func(t *testing.T) {
		querier := newQuerier(&hierarchyFederation{answerFor: "!other:remote"})
		require.Nil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"}, true))
	})
	t.Run("servers a client named leave the caches alone", func(t *testing.T) {
		federation := &hierarchyFederation{err: &fs.FederationClientError{Code: 502}}
		querier := newQuerier(federation)
		require.Nil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"}, false))
		federation.err = nil
		require.NotNil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"}, false))
		require.Equal(t, 2, federation.calls, "the failure was cached")
		time.Sleep(50 * time.Millisecond)
		_, cached := querier.Cache.GetRoomHierarchy(roomID.String())
		require.False(t, cached, "the answer was cached")
		require.False(t, querier.Cache.GetRoomHierarchyFailure(roomID.String()))
	})
}

func TestRemoteRoomAccessible(t *testing.T) {
	room := func(joinRule string, worldReadable bool, allowed ...string) fclient.RoomHierarchyRoom {
		return fclient.RoomHierarchyRoom{
			PublicRoom:     fclient.PublicRoom{RoomID: "!room:remote", JoinRule: joinRule, WorldReadable: worldReadable},
			AllowedRoomIDs: allowed,
		}
	}
	memberships := func(m map[string]string) func(string) string {
		return func(roomID string) string { return m[roomID] }
	}
	none := memberships(nil)

	for _, tc := range []struct {
		name       string
		room       fclient.RoomHierarchyRoom
		membership func(string) string
		want       bool
	}{
		{"public", room(spec.Public, false), none, true},
		{"knock", room(spec.Knock, false), none, true},
		{"knock_restricted", room(spec.KnockRestricted, false), none, true},
		{"world readable", room(spec.Invite, true), none, true},
		{"invite only", room(spec.Invite, false), none, false},
		{"invited", room(spec.Invite, false), memberships(map[string]string{"!room:remote": spec.Invite}), true},
		{"restricted, joined to an allowed room", room(spec.Restricted, false, "!a:local", "!b:local"), memberships(map[string]string{"!b:local": spec.Join}), true},
		{"restricted, invited to an allowed room", room(spec.Restricted, false, "!a:local"), memberships(map[string]string{"!a:local": spec.Invite}), false},
		{"restricted, not in an allowed room", room(spec.Restricted, false, "!a:local"), none, false},
		{"invite only lists allowed rooms", room(spec.Invite, false, "!a:local"), memberships(map[string]string{"!a:local": spec.Join}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, userMaySeeRoom(tc.room, tc.membership))
		})
	}
}
