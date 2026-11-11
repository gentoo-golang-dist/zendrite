// Copyright 2024 New Vector Ltd.
// Copyright 2017 Vector Creations Ltd
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"math"
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

type getMembershipResponse struct {
	Chunk []synctypes.ClientEvent `json:"chunk"`
}

// GetMemberships implements
//
//	GET /rooms/{roomId}/members
func GetMemberships(
	req *http.Request, device *userapi.Device, roomID string,
	syncDB storage.Database, rsAPI api.SyncRoomserverAPI, fullState fullStateObserver,
	membership, notMembership *string, at string,
) util.JSONResponse {
	userID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam("Device UserID is invalid"),
		}
	}
	queryReq := api.QueryMembershipForUserRequest{
		RoomID: roomID,
		UserID: *userID,
	}

	var queryRes api.QueryMembershipForUserResponse
	if queryErr := rsAPI.QueryMembershipForUser(req.Context(), &queryReq, &queryRes); queryErr != nil {
		util.GetLogger(req.Context()).WithError(queryErr).Error("rsAPI.QueryMembershipsForRoom failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	if !queryRes.HasBeenInRoom {
		return util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.Forbidden("You aren't a member of the room and weren't previously a member of the room."),
		}
	}

	// The members of a room joined with partial state are only known once
	// its full state is (MSC3902), so wait for that, as Synapse does.
	if err = awaitFullState(req.Context(), rsAPI, fullState, roomID); err != nil {
		util.GetLogger(req.Context()).WithError(err).Warn("Waiting for the full state of the room failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	db, err := syncDB.NewDatabaseSnapshot(req.Context())
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	defer db.Rollback() //nolint:errcheck

	atToken, err := types.NewTopologyTokenFromString(at)
	if err != nil {
		atToken = types.TopologyToken{Depth: math.MaxInt64, PDUPosition: math.MaxInt64}
		if queryRes.HasBeenInRoom && !queryRes.IsInRoom {
			// If you have left the room then this will be the members of the room when you left.
			atToken, err = db.EventPositionInTopology(req.Context(), queryRes.EventID)
			if err != nil {
				util.GetLogger(req.Context()).WithError(err).Error("unable to get 'atToken'")
				return util.JSONResponse{
					Code: http.StatusInternalServerError,
					JSON: spec.InternalServerError{},
				}
			}
		}
	}

	eventIDs, err := db.SelectMemberships(req.Context(), roomID, atToken, membership, notMembership)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("db.SelectMemberships failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	qryRes := &api.QueryEventsByIDResponse{}
	if err := rsAPI.QueryEventsByID(req.Context(), &api.QueryEventsByIDRequest{EventIDs: eventIDs, RoomID: roomID}, qryRes); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("rsAPI.QueryEventsByID failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	result := qryRes.Events

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: getMembershipResponse{synctypes.ToClientEvents(gomatrixserverlib.ToPDUs(result), synctypes.FormatAll, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) { //nolint:contextcheck
			return rsAPI.QueryUserIDForSender(req.Context(), roomID, senderID)
		})},
	}
}

// fullStateObserver tells when the sync API has stored the full state of a
// room that was joined with partial state.
type fullStateObserver interface {
	ObserveUnPartialStated(roomID string) (<-chan struct{}, func())
}

// awaitFullState waits until the sync API has the full state of a room that
// was joined with partial state (MSC3902), or the request ends.
func awaitFullState(ctx context.Context, rsAPI api.SyncRoomserverAPI, fullState fullStateObserver, roomID string) error {
	if fullState == nil {
		return nil
	}
	ch, stop := fullState.ObserveUnPartialStated(roomID)
	defer stop()
	partial, err := rsAPI.IsRoomIDPartialState(ctx, roomID)
	if err != nil || !partial {
		return err
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
