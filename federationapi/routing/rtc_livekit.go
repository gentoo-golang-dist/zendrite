// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/internal/livekit"
	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
)

// liveKitRetryAfterMS is the retry_after_ms of a rate-limited token request.
const liveKitRetryAfterMS = 1000

// liveKitTokenRequest is the body of POST /rtc/livekit/get_token over
// federation (MSC4195).
type liveKitTokenRequest struct {
	UserID   string `json:"user_id"`
	URL      string `json:"url"`
	RoomID   string `json:"room_id"`
	SlotID   string `json:"slot_id"`
	MemberID string `json:"member_id"`
}

// GetLiveKitToken implements POST /_matrix/federation/v1/rtc/livekit/get_token
// (MSC4195): it issues a token for one of this server's SFUs to a user of
// the requesting server who is joined to the room.
func GetLiveKitToken(
	httpReq *http.Request, request *fclient.FederationRequest, cfg *config.FederationAPI,
	rsAPI roomserverAPI.FederationRoomserverAPI, issuer *livekit.Issuer,
) util.JSONResponse {
	var body liveKitTokenRequest
	if err := json.Unmarshal(request.Content(), &body); err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("The request body could not be decoded.")}
	}
	if body.UserID == "" || body.URL == "" || body.RoomID == "" || body.SlotID == "" || body.MemberID == "" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("user_id, url, room_id, slot_id and member_id are required.")}
	}
	forbidden := util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("No token can be issued for this user in this room.")}
	roomID, err := spec.NewRoomID(body.RoomID)
	if err != nil {
		return forbidden
	}
	userID, err := spec.NewUserID(body.UserID, true)
	if err != nil || userID.Domain() != request.Origin() {
		return forbidden
	}
	if roomserverAPI.IsServerBannedFromRoom(httpReq.Context(), rsAPI, roomID.String(), request.Origin()) {
		return forbidden
	}
	if !issuer.Allow(string(request.Origin())) {
		return util.JSONResponse{Code: http.StatusTooManyRequests, JSON: spec.LimitExceeded("Too many token requests.", liveKitRetryAfterMS)}
	}

	var joined roomserverAPI.QueryServerJoinedToRoomResponse
	if err = rsAPI.QueryServerJoinedToRoom(httpReq.Context(), &roomserverAPI.QueryServerJoinedToRoomRequest{
		RoomID: roomID.String(), ServerName: cfg.Matrix.ServerName,
	}, &joined); err != nil {
		util.GetLogger(httpReq.Context()).WithError(err).Error("QueryServerJoinedToRoom failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !joined.RoomExists || !joined.IsInRoom {
		return forbidden
	}
	var membership roomserverAPI.QueryMembershipForUserResponse
	if err = rsAPI.QueryMembershipForUser(httpReq.Context(), &roomserverAPI.QueryMembershipForUserRequest{
		RoomID: roomID.String(), UserID: *userID,
	}, &membership); err != nil {
		util.GetLogger(httpReq.Context()).WithError(err).Error("QueryMembershipForUser failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !membership.IsInRoom || membership.Membership != spec.Join {
		return forbidden
	}

	focus := issuer.SFU(body.URL)
	if focus == nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("url is not an SFU of this server.")}
	}
	// Remote users may publish too, as MSC4195 recommends.
	jwt, err := issuer.Token(httpReq.Context(), focus, roomID.String(), body.SlotID, userID.String(), body.MemberID, true)
	if err != nil {
		util.GetLogger(httpReq.Context()).WithError(err).Error("Issuing a LiveKit token failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: map[string]string{"jwt": jwt}}
}
