// Copyright 2024 New Vector Ltd.
// Copyright 2017 Vector Creations Ltd
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/gomatrix"
	"github.com/matrix-org/util"

	appserviceAPI "codefloe.com/pat-s/zendrite/appservice/api"
	"codefloe.com/pat-s/zendrite/clientapi/httputil"
	"codefloe.com/pat-s/zendrite/internal/eventutil"
	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// joinRoomResponse is the body of a successful join.
type joinRoomResponse struct {
	RoomID string `json:"room_id"`
}

func JoinRoomByIDOrAlias(
	req *http.Request,
	device *api.Device,
	rsAPI roomserverAPI.ClientRoomserverAPI,
	profileAPI api.ClientUserAPI,
	roomIDOrAlias string,
) util.JSONResponse {
	return joinRoomByIDOrAlias(req, device, rsAPI, profileAPI, roomIDOrAlias, false)
}

// joinRoomByIDOrAlias is JoinRoomByIDOrAlias. With keepClientProfile, a
// displayname or avatar_url in the request body is kept, as for a join sent
// as an m.room.member state event, where it sets the user's profile in the
// room; the global profile only fills in what the body leaves out.
func joinRoomByIDOrAlias(
	req *http.Request,
	device *api.Device,
	rsAPI roomserverAPI.ClientRoomserverAPI,
	profileAPI api.ClientUserAPI,
	roomIDOrAlias string,
	keepClientProfile bool,
) util.JSONResponse {
	// MSC3706: Trace join timing for diagnostics
	joinStartTime := time.Now()
	logger := util.GetLogger(req.Context()).WithFields(map[string]any{
		"room_id_or_alias": roomIDOrAlias,
		"user_id":          device.UserID,
		"trace":            "join_timing",
	})
	logger.Debug("Join request received")

	// Prepare to ask the roomserver to perform the room join.
	joinReq := roomserverAPI.PerformJoinRequest{
		RoomIDOrAlias: roomIDOrAlias,
		UserID:        device.UserID,
		IsGuest:       device.AccountType == api.AccountTypeGuest,
		LocalOnly:     device.AccountType == api.AccountTypeCallGuest,
		Content:       map[string]any{},
	}

	// Check to see if any ?via= or ?server_name= query parameters
	// were given in the request.
	if serverNames, ok := req.URL.Query()["via"]; ok {
		for _, serverName := range serverNames {
			joinReq.ServerNames = append(
				joinReq.ServerNames,
				spec.ServerName(serverName),
			)
		}
	} else if serverNames, ok := req.URL.Query()["server_name"]; ok {
		for _, serverName := range serverNames {
			joinReq.ServerNames = append(
				joinReq.ServerNames,
				spec.ServerName(serverName),
			)
		}
	}

	// If content was provided in the request then include that
	// in the request. It'll get used as a part of the membership
	// event content.
	_ = httputil.UnmarshalJSONRequest(req, &joinReq.Content)

	// Work out our localpart for the client profile request.

	// Request our profile content to populate the request content with.
	profile, err := profileAPI.QueryProfile(req.Context(), device.UserID)

	switch {
	case err == nil:
		if _, ok := joinReq.Content["displayname"]; !ok || !keepClientProfile {
			joinReq.Content["displayname"] = profile.DisplayName
		}
		if _, ok := joinReq.Content["avatar_url"]; !ok || !keepClientProfile {
			joinReq.Content["avatar_url"] = profile.AvatarURL
		}
	case errors.Is(err, appserviceAPI.ErrProfileNotExists):
		util.GetLogger(req.Context()).Error("Unable to query user profile, no profile found.")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.Unknown("Unable to query user profile, no profile found."),
		}
	default:
	}

	// Ask the roomserver to perform the join.
	roomID, _, err := rsAPI.PerformJoin(req.Context(), &joinReq)
	if res, ok := roomserverAPI.PolicyServerRejection(err); ok {
		return res
	}
	var response util.JSONResponse

	var errInvalidID roomserverAPI.ErrInvalidID
	var errNotAllowed roomserverAPI.ErrNotAllowed
	var errHTTP *gomatrix.HTTPError
	var errRoomNoExists eventutil.ErrRoomNoExists
	switch {
	case err == nil: // success case
		response = util.JSONResponse{
			Code: http.StatusOK,
			JSON: joinRoomResponse{RoomID: roomID},
		}
	case errors.As(err, &errInvalidID):
		response = util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam(errInvalidID.Error()),
		}
	case errors.As(err, &errNotAllowed):
		jsonErr := spec.Forbidden(errNotAllowed.Error())
		if device.AccountType == api.AccountTypeGuest {
			jsonErr = spec.GuestAccessForbidden(errNotAllowed.Error())
		}
		response = util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: jsonErr,
		}
	case errors.As(err, &errHTTP) && errHTTP.Code >= 400 && json.Valid([]byte(errHTTP.Message)):
		// this ensures we proxy responses over federation to the client
		response = util.JSONResponse{
			Code: errHTTP.Code,
			JSON: json.RawMessage(errHTTP.Message),
		}
	case errors.As(err, &errHTTP):
		// The join failed without a remote answer to pass through, e.g.
		// because the room state it returned could not be stored.
		util.GetLogger(req.Context()).WithError(err).Warn("Join failed over federation")
		response = util.JSONResponse{
			Code: http.StatusBadGateway,
			JSON: spec.Unknown("Unable to join the room through any server"),
		}
	case errors.As(err, &errRoomNoExists):
		response = util.JSONResponse{
			Code: http.StatusNotFound,
			JSON: spec.NotFound(errRoomNoExists.Error()),
		}
	default:
		// Check if this is already a Matrix error and preserve its error code
		if resp := httputil.MatrixErrorResponse(err); resp != nil {
			response = *resp
		} else {
			response = util.JSONResponse{
				Code: http.StatusInternalServerError,
				JSON: spec.InternalServerError{},
			}
		}
	}

	// Wait as long as it takes for the join to finish (should be reasonable with MSC3706),
	// the spec doesn't mention this but it seems to be what clients expect.
	logger.WithFields(map[string]any{
		"duration_ms": time.Since(joinStartTime).Milliseconds(),
		"result_code": response.Code,
	}).Debug("Join request completed")
	return response
}
