// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
)

// extremitiesResponse is the body of an MSC4370 extremities response.
type extremitiesResponse struct {
	PrevEvents []string `json:"prev_events"`
}

// GetExtremities answers MSC4370 requests for the forward extremities of a
// room: the event IDs this server would put in the prev_events of an event
// it sent to the room now.
func GetExtremities(
	httpReq *http.Request,
	request *fclient.FederationRequest,
	rsAPI roomserverAPI.FederationRoomserverAPI,
	roomID string,
) util.JSONResponse {
	ctx := httpReq.Context()
	if _, err := spec.NewRoomID(roomID); err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Invalid room ID.")}
	}

	// As in Synapse, the membership of the requesting server is checked
	// first, so servers outside the room learn nothing of the server ACL or
	// of whether this server is still in the room. A partial-state room is
	// refused before that, since its memberships are incomplete.
	var origin roomserverAPI.QueryServerJoinedToRoomResponse
	if err := rsAPI.QueryServerJoinedToRoom(ctx, &roomserverAPI.QueryServerJoinedToRoomRequest{
		RoomID: roomID, ServerName: request.Origin(),
	}, &origin); err != nil {
		util.GetLogger(ctx).WithError(err).Error("QueryServerJoinedToRoom failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	switch {
	case !origin.RoomExists:
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Room not found.")}
	case origin.IsPartialState:
		return util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.MatrixError{ErrCode: "ORG.MATRIX.MSC3895_UNABLE_DUE_TO_PARTIAL_STATE", Err: "Unable to process request; room is in partial state during faster join resynchronization"},
		}
	case !origin.IsInRoom:
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("Host not in room.")}
	}

	// The extremities of a room this server has left are no longer current.
	var local roomserverAPI.QueryServerJoinedToRoomResponse
	if err := rsAPI.QueryServerJoinedToRoom(ctx, &roomserverAPI.QueryServerJoinedToRoomRequest{RoomID: roomID}, &local); err != nil {
		util.GetLogger(ctx).WithError(err).Error("QueryServerJoinedToRoom failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !local.IsInRoom {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("This server is not in the room.")}
	}

	var banned roomserverAPI.QueryServerBannedFromRoomResponse
	if err := rsAPI.QueryServerBannedFromRoom(ctx, &roomserverAPI.QueryServerBannedFromRoomRequest{
		RoomID: roomID, ServerName: request.Origin(),
	}, &banned); err != nil {
		util.GetLogger(ctx).WithError(err).Error("QueryServerBannedFromRoom failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if banned.Banned {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("Server is banned from room.")}
	}

	// Fetching only the create event keeps the state lookup cheap: the
	// response needs the latest events, not the room state.
	var latest roomserverAPI.QueryLatestEventsAndStateResponse
	if err := rsAPI.QueryLatestEventsAndState(ctx, &roomserverAPI.QueryLatestEventsAndStateRequest{
		RoomID:       roomID,
		StateToFetch: []gomatrixserverlib.StateKeyTuple{{EventType: spec.MRoomCreate, StateKey: ""}},
	}, &latest); err != nil {
		util.GetLogger(ctx).WithError(err).Error("QueryLatestEventsAndState failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if len(latest.LatestEvents) == 0 {
		util.GetLogger(ctx).WithField("room_id", roomID).Error("Room has no forward extremities")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: extremitiesResponse{PrevEvents: latest.LatestEvents}}
}
