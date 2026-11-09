// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/clientapi/auth"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// SendMembershipStateEvent handles PUT /rooms/{roomID}/state/m.room.member/{userID}
// when the user changes their own membership by joining, leaving or knocking.
//
// Such a request used to be stored as an ordinary room event. That is not
// what the join, leave and knock endpoints do: they ask the roomserver to
// perform the change, which is what makes a join or a knock reach a room on
// another server, and what rejects an invite to a room this server is not in.
// The raw event could do neither, since the user has no place in such a room
// to send it from, and it also skipped checks such as the one that keeps a
// user from leaving their server notices room. As Synapse does, this sends
// these requests through the same code as the membership endpoints. Invites
// are sent by sendMemberInvite, and changes to another user's membership
// (kick, ban, unban) are plain events either way.
//
// A join by a user who is already joined is not a join but a change to the
// profile in the room, such as a per-room nickname or avatar. It has to stay
// a plain event with the content the client sent: the join endpoint would
// replace the displayname and avatar_url with the user's global profile and
// send no event at all.
//
// The returned bool reports whether the request was handled. If it is false,
// the request body is still readable and the caller sends the event as usual.
func SendMembershipStateEvent(
	req *http.Request,
	device *userapi.Device,
	roomID, eventType, stateKey string,
	rsAPI api.ClientRoomserverAPI,
	profileAPI userapi.ClientUserAPI,
) (util.JSONResponse, bool) {
	if eventType != spec.MRoomMember || stateKey != device.UserID {
		return util.JSONResponse{}, false
	}
	if _, err := spec.NewRoomID(roomID); err != nil {
		return util.JSONResponse{}, false
	}
	userID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		return util.JSONResponse{}, false
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}, true
	}
	// Leave the body readable for whatever handles the request.
	req.Body = io.NopCloser(bytes.NewReader(body))
	var content map[string]any
	if err = json.Unmarshal(body, &content); err != nil {
		return util.JSONResponse{}, false
	}
	membership, _ := content["membership"].(string)
	switch membership {
	case spec.Join, spec.Knock, spec.Leave:
	default:
		return util.JSONResponse{}, false
	}

	// What the user's membership is now. An error means this server does not
	// know the room, which is what joining and knocking are for.
	queryReq := &api.QueryMembershipForUserRequest{RoomID: roomID, UserID: *userID}
	var before api.QueryMembershipForUserResponse
	if err = rsAPI.QueryMembershipForUser(req.Context(), queryReq, &before); err != nil {
		before = api.QueryMembershipForUserResponse{}
	}
	if membership == spec.Join && before.Membership == spec.Join {
		return util.JSONResponse{}, false
	}

	if resErr := callGuestEventForbidden(device, eventType, &stateKey); resErr != nil {
		return *resErr, true
	}
	// A suspended user may only leave (MSC3823), as on the membership
	// endpoints.
	if device.Suspended && membership != spec.Leave {
		return auth.UserSuspendedResponse(), true
	}

	// Sending the same request twice answers with the event it produced the
	// first time, as for any other state event.
	memberKey := stateKey
	if before.SenderID != nil {
		memberKey = string(*before.SenderID)
	}
	if eventID, ok := stateEqual(req.Context(), rsAPI, spec.MRoomMember, memberKey, roomID, content); ok {
		return util.JSONResponse{Code: http.StatusOK, JSON: sendEventResponse{eventID}}, true
	}

	var res util.JSONResponse
	var leaveRes api.PerformLeaveResponse
	switch membership {
	case spec.Join:
		res = joinRoomByIDOrAlias(req, device, rsAPI, profileAPI, roomID, true)
	case spec.Knock:
		res = KnockRoomByIDOrAlias(req, device, rsAPI, profileAPI, roomID)
	case spec.Leave:
		res, leaveRes = leaveRoomByID(req, device, rsAPI, roomID)
	}
	if res.Code != http.StatusOK {
		return res, true
	}

	// The endpoints answer with the room ID or nothing; this one answers with
	// the ID of the membership event the change produced.
	var after api.QueryMembershipForUserResponse
	if err = rsAPI.QueryMembershipForUser(req.Context(), queryReq, &after); err != nil {
		util.GetLogger(req.Context()).WithError(err).WithField("room_id", roomID).Error(
			"membership changed but looking up its event failed",
		)
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}, true
	}
	eventID := after.EventID
	if eventID == "" {
		// Rejecting an invite or withdrawing a knock on a room this server is
		// not in is done through a server that is: this one stores no leave
		// event and just forgets the membership. Answer with the leave event
		// sent to that server, or, if none of them took it (the invite or
		// knock is retired here anyway), with the invite or knock that ended.
		eventID = leaveRes.EventID
	}
	if eventID == "" {
		eventID = before.EventID
	}
	if eventID == "" {
		util.GetLogger(req.Context()).WithField("room_id", roomID).Error(
			"membership changed but its event was not found",
		)
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}, true
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: sendEventResponse{eventID}}, true
}
