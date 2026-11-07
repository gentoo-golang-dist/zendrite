// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

type UserDirectoryResponse struct {
	Results []authtypes.FullyQualifiedProfile `json:"results"`
	Limited bool                              `json:"limited"`
}

func SearchUserDirectory(
	ctx context.Context,
	device *userapi.Device,
	rsAPI api.ClientRoomserverAPI,
	provider userapi.QuerySearchProfilesAPI,
	searchString string,
	limit int,
	federation fclient.FederationClient,
	localServerName spec.ServerName,
) util.JSONResponse {
	if limit < 10 { //nolint:mnd
		limit = 10
	}

	// The spec requires the search to be case-insensitive on user IDs and display names.
	searchString = strings.ToLower(searchString)
	matches := func(s string) bool {
		return strings.Contains(strings.ToLower(s), searchString)
	}

	results := map[string]authtypes.FullyQualifiedProfile{}
	response := &UserDirectoryResponse{
		Results: []authtypes.FullyQualifiedProfile{},
		Limited: false,
	}

	// Get users we share a room with
	knownUsersReq := &api.QueryKnownUsersRequest{
		UserID: device.UserID,
		Limit:  limit,
	}
	knownUsersRes := &api.QueryKnownUsersResponse{}
	if err := rsAPI.QueryKnownUsers(ctx, knownUsersReq, knownUsersRes); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return util.ErrorResponse(fmt.Errorf("rsAPI.QueryKnownUsers: %w", err))
	}

knownUsersLoop:
	for _, profile := range knownUsersRes.Users {
		if len(results) == limit {
			response.Limited = true
			break
		}
		userID := profile.UserID
		// The requester is not part of their own search results.
		if userID == device.UserID {
			continue
		}
		// get the full profile of the local user
		localpart, serverName, _ := gomatrixserverlib.SplitID('@', userID)
		if serverName == localServerName {
			userReq := &userapi.QuerySearchProfilesRequest{
				SearchString: localpart,
				Limit:        limit,
			}
			userRes := &userapi.QuerySearchProfilesResponse{}
			if err := provider.QuerySearchProfiles(ctx, userReq, userRes); err != nil {
				return util.ErrorResponse(fmt.Errorf("userAPI.QuerySearchProfiles: %w", err))
			}
			for _, p := range userRes.Profiles {
				// The profile search matches substrings, so skip profiles of other users.
				if p.Localpart != localpart || p.ServerName != string(serverName) {
					continue
				}
				if matches(p.DisplayName) || matches(userID) {
					profile.DisplayName = p.DisplayName
					profile.AvatarURL = p.AvatarURL
					results[userID] = profile
					if len(results) == limit {
						response.Limited = true
						break knownUsersLoop
					}
				}
			}
		} else {
			// If the username already contains the search string, don't bother hitting federation.
			// This will result in missing avatars and displaynames, but saves the federation roundtrip.
			if matches(userID) {
				results[userID] = profile
				if len(results) == limit {
					response.Limited = true
					break knownUsersLoop
				}
				continue
			}
			// TODO: We should probably cache/store this
			fedProfile, fedErr := federation.LookupProfile(ctx, localServerName, serverName, userID, "")
			if fedErr != nil {
				var x gomatrix.HTTPError
				if errors.As(fedErr, &x) {
					if x.Code == http.StatusNotFound {
						continue
					}
				}
			}
			if matches(fedProfile.DisplayName) {
				profile.DisplayName = fedProfile.DisplayName
				profile.AvatarURL = fedProfile.AvatarURL
				results[userID] = profile
				if len(results) == limit {
					response.Limited = true
					break knownUsersLoop
				}
			}
		}
	}

	for _, result := range results {
		response.Results = append(response.Results, result)
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: response,
	}
}
