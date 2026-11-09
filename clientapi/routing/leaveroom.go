// Copyright 2024 New Vector Ltd.
// Copyright 2017 Vector Creations Ltd
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"bytes"
	"errors"
	"io"
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/clientapi/httputil"
	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

func LeaveRoomByID(
	req *http.Request,
	device *api.Device,
	rsAPI roomserverAPI.ClientRoomserverAPI,
	roomID string,
) util.JSONResponse {
	res, _ := leaveRoomByID(req, device, rsAPI, roomID)
	return res
}

// leaveRoomByID is LeaveRoomByID, also returning the roomserver's answer, which
// names the leave event when it was sent through another server.
func leaveRoomByID(
	req *http.Request,
	device *api.Device,
	rsAPI roomserverAPI.ClientRoomserverAPI,
	roomID string,
) (util.JSONResponse, roomserverAPI.PerformLeaveResponse) {
	leaveRes := roomserverAPI.PerformLeaveResponse{}
	userID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam("device userID is invalid"),
		}, leaveRes
	}

	// The body, including the reason for leaving, is optional.
	var body struct {
		Reason string `json:"reason"`
	}
	if req.Body != nil {
		raw, readErr := io.ReadAll(req.Body)
		if readErr != nil {
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}, leaveRes
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			if resErr := httputil.UnmarshalJSON(raw, &body); resErr != nil {
				return *resErr, leaveRes
			}
		}
	}

	// Prepare to ask the roomserver to perform the room leave.
	leaveReq := roomserverAPI.PerformLeaveRequest{
		RoomID: roomID,
		Leaver: *userID,
		Reason: body.Reason,
	}

	// Ask the roomserver to perform the leave.
	if err := rsAPI.PerformLeave(req.Context(), &leaveReq, &leaveRes); err != nil {
		if res, ok := roomserverAPI.PolicyServerRejection(err); ok {
			return res, leaveRes
		}
		if leaveRes.Code != 0 {
			return util.JSONResponse{
				Code: leaveRes.Code,
				JSON: spec.LeaveServerNoticeError(),
			}, leaveRes
		}
		// Check if this is already a Matrix error and preserve its error code
		if resp := httputil.MatrixErrorResponse(err); resp != nil {
			return *resp, leaveRes
		}
		// Check for specific error types from roomserver
		{
			var e roomserverAPI.ErrNotAllowed
			switch {
			case errors.As(err, &e):
				return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden(e.Error())}, leaveRes
			default:
				return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.Unknown(err.Error())}, leaveRes
			}
		}
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct{}{},
	}, leaveRes
}
