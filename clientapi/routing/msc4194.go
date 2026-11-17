// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"bytes"
	"io"
	"net/http"
	"strconv"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/clientapi/auth"
	"codefloe.com/pat-s/zendrite/clientapi/httputil"
	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

const (
	// RedactUserEventsDefaultLimit is the MSC4194 default for limit.
	redactUserEventsDefaultLimit = 25
	// RedactUserEventsMaxLimit caps how many events one request redacts.
	// MSC4194 lets a server redact fewer than asked for; the client calls
	// again while is_more_events is true.
	redactUserEventsMaxLimit = 100
)

type redactUserEventsRequest struct {
	Reason string `json:"reason"`
}

type redactUserEventsCounts struct {
	Total      int `json:"total"`
	SoftFailed int `json:"soft_failed"`
}

type redactUserEventsResponse struct {
	IsMoreEvents   bool                   `json:"is_more_events"`
	RedactedEvents redactUserEventsCounts `json:"redacted_events"`
}

// RedactUserEvents implements
// POST /_matrix/client/unstable/org.matrix.msc4194/rooms/{roomId}/redact/user/{userId},
// which redacts the events a user sent to a room, newest first, soft-failed
// events included (MSC4194). It sends one m.room.redaction per event, as the
// requester, and skips events that are already redacted, so that a client
// calling again while is_more_events is true always makes progress.
func RedactUserEvents(
	req *http.Request, device *userapi.Device, roomID, userID string,
	rsAPI roomserverAPI.ClientRoomserverAPI,
) util.JSONResponse {
	ctx := req.Context()
	validRoomID, err := spec.NewRoomID(roomID)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam("roomId is invalid"),
		}
	}
	targetUserID, err := spec.NewUserID(userID, true)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam("userId is invalid"),
		}
	}
	limit, resErr := parseRedactUserEventsLimit(req)
	if resErr != nil {
		return *resErr
	}
	var body redactUserEventsRequest
	if resErr = unmarshalOptionalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}

	deviceUserID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	if resErr = checkMemberInRoom(ctx, rsAPI, *deviceUserID, roomID); resErr != nil {
		return *resErr
	}
	senderID, err := rsAPI.QuerySenderIDForUser(ctx, *validRoomID, *deviceUserID)
	if err != nil || senderID == nil {
		util.GetLogger(ctx).WithError(err).WithField("roomID", roomID).Error("missing sender ID for user, despite having membership")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	targetSenderID, err := rsAPI.QuerySenderIDForUser(ctx, *validRoomID, *targetUserID)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("rsAPI.QuerySenderIDForUser failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	// Users may redact their own events, also while suspended (MSC3823).
	// Anyone else needs the room's redact power level, as for /redact.
	if targetSenderID == nil || *targetSenderID != *senderID {
		if device.Suspended {
			return auth.UserSuspendedResponse()
		}
		if !isPrivilegedCreator(ctx, rsAPI, roomID, *senderID) {
			if resErr = hasRedactPowerLevel(ctx, rsAPI, roomID, *senderID); resErr != nil {
				return *resErr
			}
		}
	}

	res := redactUserEventsResponse{}
	if targetSenderID == nil {
		// In a room with pseudo IDs a user who never joined has sent nothing.
		return util.JSONResponse{Code: http.StatusOK, JSON: res}
	}

	// One more event than redacted tells whether any are left.
	events, err := rsAPI.QueryUnredactedEventsBySender(ctx, *validRoomID, *targetSenderID, limit+1)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("rsAPI.QueryUnredactedEventsBySender failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	if len(events) > limit {
		res.IsMoreEvents = true
		events = events[:limit]
	}
	for _, ev := range events {
		// The client gave up; what has been redacted so far stays redacted
		// and a later request carries on from there.
		if ctx.Err() != nil {
			return util.JSONResponse{Code: http.StatusOK, JSON: res}
		}
		if _, resErr = sendRedactionEvent(ctx, device, *validRoomID, *senderID, ev.EventID, redactionContent{Reason: body.Reason}, rsAPI); resErr != nil {
			return *resErr
		}
		res.RedactedEvents.Total++
		if ev.Undelivered {
			res.RedactedEvents.SoftFailed++
		}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: res}
}

// parseRedactUserEventsLimit reads the limit query parameter of MSC4194,
// capped at redactUserEventsMaxLimit.
func parseRedactUserEventsLimit(req *http.Request) (int, *util.JSONResponse) {
	raw := req.URL.Query().Get("limit")
	if raw == "" {
		return redactUserEventsDefaultLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return 0, &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam("limit must be a positive integer"),
		}
	}
	return min(limit, redactUserEventsMaxLimit), nil
}

// unmarshalOptionalJSONRequest decodes the request body into iface, leaving
// it unchanged when the body is empty.
func unmarshalOptionalJSONRequest(req *http.Request, iface any) *util.JSONResponse {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("io.ReadAll failed")
		return &util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	return httputil.UnmarshalJSON(body, iface)
}
