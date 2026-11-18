// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/clientapi/httputil"
	"codefloe.com/pat-s/zendrite/internal/livekit"
	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// The federation paths of the LiveKit token endpoint (MSC4195): the stable
// one, then the unstable one for servers that only know that.
var liveKitTokenFederationPaths = []string{
	"/_matrix/federation/v1/rtc/livekit/get_token",
	"/_matrix/federation/unstable/io.element.msc4195/rtc/livekit/get_token",
}

// liveKitTokenRequest is the body of POST /rtc/livekit/get_token (MSC4195).
type liveKitTokenRequest struct {
	ServerName string `json:"server_name,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	URL        string `json:"url"`
	RoomID     string `json:"room_id"`
	SlotID     string `json:"slot_id"`
	MemberID   string `json:"member_id"`
}

// GetLiveKitToken implements POST /_matrix/client/v1/rtc/livekit/get_token
// (MSC4195): a token for one of this server's SFUs, or, through the other
// server, for one of its SFUs. The user must be joined to the room.
func GetLiveKitToken(
	req *http.Request, device *userapi.Device, cfg *config.ClientAPI,
	rsAPI roomserverAPI.ClientRoomserverAPI, federation fclient.FederationClient, issuer *livekit.Issuer,
) util.JSONResponse {
	var body liveKitTokenRequest
	if resErr := httputil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	if body.URL == "" || body.RoomID == "" || body.SlotID == "" || body.MemberID == "" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("url, room_id, slot_id and member_id are required.")}
	}
	forbidden := util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("No token can be issued for this room.")}
	roomID, err := spec.NewRoomID(body.RoomID)
	if err != nil {
		return forbidden
	}
	userID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		return forbidden
	}
	var membership roomserverAPI.QueryMembershipForUserResponse
	if err = rsAPI.QueryMembershipForUser(req.Context(), &roomserverAPI.QueryMembershipForUserRequest{
		RoomID: roomID.String(), UserID: *userID,
	}, &membership); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("QueryMembershipForUser failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !membership.IsInRoom || membership.Membership != spec.Join {
		return forbidden
	}

	serverName := spec.ServerName(body.ServerName)
	if serverName == "" || cfg.Matrix.IsLocalServerName(serverName) {
		focus := issuer.SFU(body.URL)
		if focus == nil {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("url is not an SFU of this server.")}
		}
		jwt, tokenErr := issuer.Token(req.Context(), focus, roomID.String(), body.SlotID, userID.String(), body.MemberID, true)
		if tokenErr != nil {
			util.GetLogger(req.Context()).WithError(tokenErr).Error("Issuing a LiveKit token failed")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
		return util.JSONResponse{Code: http.StatusOK, JSON: map[string]string{"jwt": jwt}}
	}

	var joined roomserverAPI.QueryServerJoinedToRoomResponse
	if err = rsAPI.QueryServerJoinedToRoom(req.Context(), &roomserverAPI.QueryServerJoinedToRoomRequest{
		RoomID: roomID.String(), ServerName: serverName,
	}, &joined); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("QueryServerJoinedToRoom failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !joined.IsInRoom {
		return forbidden
	}
	remote := body
	remote.ServerName = ""
	remote.UserID = userID.String()
	return remoteLiveKitToken(req.Context(), cfg, federation, serverName, remote)
}

// remoteLiveKitToken asks another server for a token for one of its SFUs.
// Its 403 and 400 errors reach the client as they are; any other failure is
// 502 M_UNKNOWN, as MSC4195 asks.
func remoteLiveKitToken(
	ctx context.Context, cfg *config.ClientAPI, federation fclient.FederationClient,
	destination spec.ServerName, body liveKitTokenRequest,
) util.JSONResponse {
	unknown := util.JSONResponse{Code: http.StatusBadGateway, JSON: spec.Unknown("The other server could not issue a token.")}
	for i, path := range liveKitTokenFederationPaths {
		fedReq := fclient.NewFederationRequest(http.MethodPost, cfg.Matrix.ServerName, destination, path)
		if err := fedReq.SetContent(body); err != nil {
			return unknown
		}
		if err := fedReq.Sign(cfg.Matrix.ServerName, cfg.Matrix.KeyID, cfg.Matrix.PrivateKey); err != nil {
			return unknown
		}
		httpReq, err := fedReq.HTTPRequest()
		if err != nil {
			return unknown
		}
		var res struct {
			JWT string `json:"jwt"`
		}
		err = federation.DoRequestAndParseResponse(ctx, httpReq, &res)
		if err == nil {
			if res.JWT != "" {
				return util.JSONResponse{Code: http.StatusOK, JSON: map[string]string{"jwt": res.JWT}}
			}
			util.GetLogger(ctx).WithField("server_name", destination).Warn("Another server answered a LiveKit token request without a token")
			return unknown
		}
		var httpErr spec.HTTPError
		if !errors.As(err, &httpErr) {
			util.GetLogger(ctx).WithError(err).WithField("server_name", destination).Warn("Asking another server for a LiveKit token failed")
			return unknown
		}
		var matrixErr spec.MatrixError
		_ = json.Unmarshal(httpErr.Contents, &matrixErr)
		// A server that does not know this path may answer 404, 405 or 400
		// M_UNRECOGNIZED: try the next one.
		unrecognized := httpErr.Code == http.StatusNotFound || httpErr.Code == http.StatusMethodNotAllowed ||
			matrixErr.ErrCode == spec.ErrorUnrecognized
		if unrecognized && i < len(liveKitTokenFederationPaths)-1 {
			continue
		}
		if (httpErr.Code == http.StatusForbidden || httpErr.Code == http.StatusBadRequest) && !unrecognized && matrixErr.ErrCode != "" {
			return util.JSONResponse{Code: httpErr.Code, JSON: matrixErr}
		}
		return unknown
	}
	return unknown
}
