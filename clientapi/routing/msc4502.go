// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// isJoinedResponse is the body of GET /rooms/{roomId}/is_joined (MSC4502).
type isJoinedResponse struct {
	Joined bool `json:"joined"`
}

// hasRoomMembershipScope reports whether the requester may query any room's
// membership (MSC4502): either its OAuth 2.0 access token carries the scope
// (MSC3861), or it is an application service whose registration lists it.
// Server admins get no special treatment, as in Synapse.
func hasRoomMembershipScope(device *userapi.Device, cfg *config.ClientAPI) bool {
	if device.HasScope(config.ScopeQueryRoomMembership) {
		return true
	}
	if device.AppserviceID == "" {
		return false
	}
	for i := range cfg.Derived.ApplicationServices {
		as := &cfg.Derived.ApplicationServices[i]
		if as.ID == device.AppserviceID {
			return as.HasScope(config.ScopeQueryRoomMembership)
		}
	}
	return false
}

// GetRoomIsJoined implements
// GET /_matrix/client/unstable/io.element.msc4502/rooms/{roomId}/is_joined,
// which tells whether one user, or any user of one server, is joined to a
// room, without the caller having to be in the room (MSC4502).
func GetRoomIsJoined(
	req *http.Request, device *userapi.Device, roomID string,
	cfg *config.ClientAPI, rsAPI roomserverAPI.ClientRoomserverAPI,
) util.JSONResponse {
	if !hasRoomMembershipScope(device, cfg) {
		return util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.Forbidden("Missing " + config.ScopeQueryRoomMembership + " scope"),
		}
	}
	if _, err := spec.NewRoomID(roomID); err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam("Invalid room ID"),
		}
	}

	query := req.URL.Query()
	_, hasMXID := query["mxid"]
	_, hasServerName := query["server_name"]
	if hasMXID == hasServerName {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.MissingParam("Exactly one of 'mxid' or 'server_name' query parameters must be given"),
		}
	}

	if hasMXID {
		mxid := query.Get("mxid")
		userID, err := spec.NewUserID(mxid, true)
		if err != nil {
			return util.JSONResponse{
				Code: http.StatusBadRequest,
				JSON: spec.InvalidParam("Invalid MXID: " + mxid),
			}
		}
		var res roomserverAPI.QueryMembershipForUserResponse
		if err = rsAPI.QueryMembershipForUser(req.Context(), &roomserverAPI.QueryMembershipForUserRequest{
			RoomID: roomID,
			UserID: *userID,
		}, &res); err != nil {
			util.GetLogger(req.Context()).WithError(err).Error("rsAPI.QueryMembershipForUser failed")
			return util.JSONResponse{
				Code: http.StatusInternalServerError,
				JSON: spec.InternalServerError{},
			}
		}
		return util.JSONResponse{
			Code: http.StatusOK,
			JSON: isJoinedResponse{Joined: res.RoomExists && res.Membership == spec.Join},
		}
	}

	serverName := spec.ServerName(query.Get("server_name"))
	if _, _, ok := spec.ParseAndValidateServerName(serverName); !ok {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam("Invalid server name: " + string(serverName)),
		}
	}
	var res roomserverAPI.QueryServerJoinedToRoomResponse
	if err := rsAPI.QueryServerJoinedToRoom(req.Context(), &roomserverAPI.QueryServerJoinedToRoomRequest{
		RoomID:     roomID,
		ServerName: serverName,
	}, &res); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("rsAPI.QueryServerJoinedToRoom failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: isJoinedResponse{Joined: res.RoomExists && res.IsInRoom},
	}
}
