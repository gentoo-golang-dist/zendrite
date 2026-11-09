// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"net/http"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// sendMemberInvite sends an invite that a client submitted as a raw
// m.room.member state event, through the same roomserver flow as the invite
// endpoint, and returns the ID of the resulting event. Submitting the event
// as an ordinary room event would only store it: a remote invitee's server
// would never be asked to accept the invite, so the invite would never arrive.
func sendMemberInvite(
	ctx context.Context,
	device *userapi.Device,
	roomID, stateKey string,
	content map[string]any,
	evTime time.Time,
	txnAndSessionID *api.TransactionID,
	cfg *config.ClientAPI,
	rsAPI api.ClientRoomserverAPI,
) (string, *util.JSONResponse) {
	validRoomID, err := spec.NewRoomID(roomID)
	if err != nil {
		return "", &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam("RoomID is invalid"),
		}
	}
	// In rooms with pseudo IDs the state key is the invitee's sender ID.
	invitee, err := spec.NewUserID(stateKey, true)
	if err != nil {
		invitee, err = rsAPI.QueryUserIDForSender(ctx, *validRoomID, spec.SenderID(stateKey))
		if err != nil || invitee == nil {
			return "", &util.JSONResponse{
				Code: http.StatusBadRequest,
				JSON: spec.InvalidParam("state_key is not a valid user ID"),
			}
		}
	}

	reason, _ := content["reason"].(string)
	res, err := sendInviteWithContent(
		ctx, device, roomID, invitee.String(), reason, content, txnAndSessionID, cfg, rsAPI, evTime,
	)
	if err != nil {
		return "", &res
	}

	// PerformInvite does not return the event, but it has been applied to
	// the room state by now, and the caller holds the user's send lock.
	memberKey := stateKey
	if senderID, sErr := rsAPI.QuerySenderIDForUser(ctx, *validRoomID, *invitee); sErr == nil && senderID != nil {
		memberKey = string(*senderID)
	}
	tuple := gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomMember, StateKey: memberKey}
	stateRes := api.QueryCurrentStateResponse{}
	if err = rsAPI.QueryCurrentState(ctx, &api.QueryCurrentStateRequest{
		RoomID:      roomID,
		StateTuples: []gomatrixserverlib.StateKeyTuple{tuple},
	}, &stateRes); err != nil || stateRes.StateEvents[tuple] == nil {
		util.GetLogger(ctx).WithError(err).WithField("room_id", roomID).Error("invite sent but its event was not found")
		return "", &util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	return stateRes.StateEvents[tuple].EventID(), nil
}
