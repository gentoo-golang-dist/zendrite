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
	errs  map[spec.ServerName]error
	calls int
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
		require.Nil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"}))
		eventuallyCached(t, querier, roomHierarchyRefusalKey(t.Context(), querier, *roomID, nil, []string{"remote"}))
		require.False(t, querier.Cache.GetRoomHierarchyFailure(roomID.String()))
		require.Nil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"}))
		require.Equal(t, 1, federation.calls)
	})
	t.Run("an unreachable server is cached for the room", func(t *testing.T) {
		federation := &hierarchyFederation{err: &fs.FederationClientError{Code: 502}}
		querier := newQuerier(federation)
		require.Nil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"}))
		eventuallyCached(t, querier, roomID.String())
		federation.err = nil
		require.Nil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"}))
		require.Equal(t, 1, federation.calls)
	})
	t.Run("a refusal does not hide an unreachable server", func(t *testing.T) {
		federation := &hierarchyFederation{errs: map[spec.ServerName]error{
			"refusing": &fs.FederationClientError{Code: 404}, "down": &fs.FederationClientError{Code: 502},
		}}
		querier := newQuerier(federation)
		require.Nil(t, federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"refusing", "down"}))
		eventuallyCached(t, querier, roomID.String())
	})
	t.Run("an answer is returned", func(t *testing.T) {
		querier := newQuerier(&hierarchyFederation{})
		res := federatedRoomInfo(t.Context(), querier, caller, false, *roomID, nil, []string{"remote"})
		require.NotNil(t, res)
		require.Equal(t, roomID.String(), res.Room.RoomID)
	})
}
