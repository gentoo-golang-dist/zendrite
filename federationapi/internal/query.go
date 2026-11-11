package internal

import (
	"context"
	"fmt"
	"slices"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/federationapi/api"
)

// QueryJoinedHostServerNamesInRoom implements api.FederationInternalAPI.
func (f *FederationInternalAPI) QueryJoinedHostServerNamesInRoom(
	ctx context.Context,
	request *api.QueryJoinedHostServerNamesInRoomRequest,
	response *api.QueryJoinedHostServerNamesInRoomResponse,
) (err error) {
	joinedHosts, err := f.db.GetJoinedHostsForRooms(ctx, []string{request.RoomID}, request.ExcludeSelf, request.ExcludeBlacklisted)
	if err != nil {
		return
	}
	response.ServerNames = joinedHosts

	// While a room has partial state, its memberships are not known, so the
	// servers in it are approximated by those the resident server named
	// when it was joined, as Synapse does (MSC3902).
	partialServers, err := f.partialStateServers(ctx, request.RoomID)
	if err != nil {
		return err
	}
	for _, server := range partialServers {
		if request.ExcludeSelf && f.cfg.Matrix.IsLocalServerName(server) {
			continue
		}
		if request.ExcludeBlacklisted {
			if blacklisted, berr := f.db.IsServerBlacklisted(server); berr == nil && blacklisted {
				continue
			}
		}
		if !slices.Contains(response.ServerNames, server) {
			response.ServerNames = append(response.ServerNames, server)
		}
	}
	return nil
}

// partialStateServers returns the servers the resident server named when the
// room was joined with partial state, or nothing if the room has full state.
func (f *FederationInternalAPI) partialStateServers(ctx context.Context, roomID string) ([]spec.ServerName, error) {
	if f.rsAPI == nil {
		return nil, nil
	}
	validRoomID, err := spec.NewRoomID(roomID)
	if err != nil {
		return nil, nil //nolint:nilerr // an invalid room ID has no servers
	}
	roomInfo, err := f.rsAPI.QueryRoomInfo(ctx, *validRoomID)
	if err != nil || roomInfo == nil {
		return nil, err
	}
	partial, err := f.rsAPI.IsRoomPartialState(ctx, roomInfo.RoomNID)
	if err != nil || !partial {
		return nil, err
	}
	servers, err := f.rsAPI.GetPartialStateServers(ctx, roomInfo.RoomNID)
	if err != nil {
		return nil, err
	}
	result := make([]spec.ServerName, 0, len(servers))
	for _, server := range servers {
		result = append(result, spec.ServerName(server))
	}
	return result, nil
}

func (a *FederationInternalAPI) fetchServerKeysDirectly(ctx context.Context, serverName spec.ServerName) (*gomatrixserverlib.ServerKeys, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second*30) //nolint:mnd
	defer cancel()
	ires, err := a.doFederationRequest(serverName, func() (any, error) { //nolint:contextcheck
		return a.federation.GetServerKeys(ctx, serverName)
	}, true)
	if err != nil {
		return nil, err
	}
	sks, ok := ires.(gomatrixserverlib.ServerKeys)
	if !ok {
		return nil, fmt.Errorf("unexpected response type for GetServerKeys")
	}
	return &sks, nil
}

func (a *FederationInternalAPI) fetchServerKeysFromCache(
	ctx context.Context, req *api.QueryServerKeysRequest,
) ([]gomatrixserverlib.ServerKeys, error) {
	var results []gomatrixserverlib.ServerKeys

	// We got a request for _all_ server keys, return them.
	if len(req.KeyIDToCriteria) == 0 {
		serverKeysResponses, _ := a.db.GetNotaryKeys(ctx, req.ServerName, []gomatrixserverlib.KeyID{})
		if len(serverKeysResponses) == 0 {
			return nil, fmt.Errorf("failed to find server key response for server %s", req.ServerName)
		}
		return serverKeysResponses, nil
	}
	for keyID, criteria := range req.KeyIDToCriteria {
		serverKeysResponses, _ := a.db.GetNotaryKeys(ctx, req.ServerName, []gomatrixserverlib.KeyID{keyID})
		if len(serverKeysResponses) == 0 {
			return nil, fmt.Errorf("failed to find server key response for key ID %s", keyID)
		}
		// we should only get 1 result as we only gave 1 key ID
		sk := serverKeysResponses[0]
		util.GetLogger(ctx).Infof("fetchServerKeysFromCache: minvalid:%v  keys: %+v", criteria.MinimumValidUntilTS, sk)
		if criteria.MinimumValidUntilTS != 0 {
			// check if it's still valid. if they have the same value that's also valid
			if sk.ValidUntilTS < criteria.MinimumValidUntilTS {
				return nil, fmt.Errorf(
					"found server response for key ID %s but it is no longer valid, min: %v valid_until: %v",
					keyID, criteria.MinimumValidUntilTS, sk.ValidUntilTS,
				)
			}
		}
		results = append(results, sk)
	}
	return results, nil
}

func (a *FederationInternalAPI) QueryServerKeys(
	ctx context.Context, req *api.QueryServerKeysRequest, res *api.QueryServerKeysResponse,
) error {
	// attempt to satisfy the entire request from the cache first
	results, err := a.fetchServerKeysFromCache(ctx, req)
	if err == nil {
		// satisfied entirely from cache, return it
		res.ServerKeys = results
		return nil
	}
	util.GetLogger(ctx).WithField("server", req.ServerName).WithError(err).Warn("notary: failed to satisfy keys request entirely from cache, hitting direct")

	serverKeys, err := a.fetchServerKeysDirectly(ctx, req.ServerName)
	if err != nil {
		// try to load as much as we can from the cache in a best effort basis
		util.GetLogger(ctx).WithField("server", req.ServerName).WithError(err).Warn("notary: failed to ask server for keys, returning best effort keys")
		serverKeysResponses, dbErr := a.db.GetNotaryKeys(ctx, req.ServerName, req.KeyIDs())
		if dbErr != nil {
			return fmt.Errorf("notary: server returned %w, and db returned %w", err, dbErr)
		}
		res.ServerKeys = serverKeysResponses
		return nil
	}
	// cache it!
	if err = a.db.UpdateNotaryKeys(context.Background(), req.ServerName, *serverKeys); err != nil { //nolint:contextcheck
		// non-fatal, still return the response
		util.GetLogger(ctx).WithError(err).Warn("failed to UpdateNotaryKeys")
	}
	res.ServerKeys = []gomatrixserverlib.ServerKeys{*serverKeys}
	return nil
}
