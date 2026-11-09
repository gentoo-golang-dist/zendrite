// Copyright 2024 New Vector Ltd.
// Copyright 2022 The Matrix.org Foundation C.I.C.
//
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
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/internal"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// GetEvent implements
//
//	GET /_matrix/client/r0/rooms/{roomId}/event/{eventId}
//
// https://spec.matrix.org/v1.4/client-server-api/#get_matrixclientv3roomsroomideventeventid
func GetEvent(
	req *http.Request,
	device *userapi.Device,
	rawRoomID string,
	eventID string,
	cfg *config.SyncAPI,
	syncDB storage.Database,
	rsAPI api.SyncRoomserverAPI,
) util.JSONResponse {
	ctx := req.Context()
	db, err := syncDB.NewDatabaseSnapshot(ctx)
	logger := util.GetLogger(ctx).WithFields(logrus.Fields{
		"event_id": eventID,
		"room_id":  rawRoomID,
	})
	if err != nil {
		logger.WithError(err).Error("GetEvent: syncDB.NewDatabaseTransaction failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	defer db.Rollback() //nolint:errcheck

	roomID, err := spec.NewRoomID(rawRoomID)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam("invalid room ID"),
		}
	}

	events, err := db.EventsForDevice(ctx, device, []string{eventID}, rsAPI)
	if err != nil {
		logger.WithError(err).Error("GetEvent: syncDB.EventsForDevice failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	// A client may fetch an event it has just sent before the sync API has
	// received it from the roomserver. Wait for it rather than answer 404.
	// Likewise, the roomserver redacts an event before the redaction request
	// returns, and the sync API only once it receives the redaction. Wait for
	// it rather than serve content that has already been redacted.
	waited := false
	switch {
	case len(events) == 0:
		waited = waitForOwnEvent(ctx, syncDB, rsAPI, device, *roomID, eventID)
	case len(events) == 1 && !isRedacted(events[0].PDU):
		waited = waitForRedaction(ctx, syncDB, rsAPI, *roomID, eventID)
	}
	if waited {
		db.Rollback() //nolint:errcheck
		if db, err = syncDB.NewDatabaseSnapshot(ctx); err != nil {
			logger.WithError(err).Error("GetEvent: syncDB.NewDatabaseSnapshot failed")
			return util.JSONResponse{
				Code: http.StatusInternalServerError,
				JSON: spec.InternalServerError{},
			}
		}
		defer db.Rollback() //nolint:errcheck
		if events, err = db.EventsForDevice(ctx, device, []string{eventID}, rsAPI); err != nil {
			logger.WithError(err).Error("GetEvent: syncDB.EventsForDevice failed")
			return util.JSONResponse{
				Code: http.StatusInternalServerError,
				JSON: spec.InternalServerError{},
			}
		}
	}

	// The requested event does not exist in our database
	if len(events) == 0 {
		logger.Debugf("GetEvent: requested event doesn't exist locally")
		return util.JSONResponse{
			Code: http.StatusNotFound,
			JSON: spec.NotFound("The event was not found or you do not have permission to read this event"),
		}
	}

	// If the request is coming from an appservice, get the user from the request
	rawUserID := device.UserID
	if asUserID := req.FormValue("user_id"); device.AppserviceID != "" && asUserID != "" {
		rawUserID = asUserID
	}

	userID, err := spec.NewUserID(rawUserID, true)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("invalid device.UserID")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	// Apply history visibility to determine if the user is allowed to view the event
	events, err = internal.ApplyHistoryVisibilityFilter(ctx, db, rsAPI, events, nil, *userID, "event")
	if err != nil {
		logger.WithError(err).Error("GetEvent: internal.ApplyHistoryVisibilityFilter failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	// We only ever expect there to be one event
	if len(events) != 1 {
		// 0 events -> not allowed to view event; > 1 events -> something that shouldn't happen
		logger.WithField("event_count", len(events)).Debug("GetEvent: can't return the requested event")
		return util.JSONResponse{
			Code: http.StatusNotFound,
			JSON: spec.NotFound("The event was not found or you do not have permission to read this event"),
		}
	}

	clientEvent, err := synctypes.ToClientEvent(events[0], synctypes.FormatAll, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
		return rsAPI.QueryUserIDForSender(ctx, roomID, senderID)
	})
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).WithField("senderID", events[0].SenderID()).WithField("roomID", *roomID).Error("Failed converting to ClientEvent")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	bundler, err := internal.NewRelationBundler(ctx, db, rsAPI, *userID)
	if err == nil {
		err = bundler.BundleEvent(ctx, events[0], clientEvent, synctypes.FormatAll)
	}
	if err != nil {
		logger.WithError(err).Error("GetEvent: failed to bundle relations")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: *clientEvent,
	}
}

// ownEventWait bounds how long GetEvent waits for an event the requester
// sent to reach the sync API.
const ownEventWait = 5 * time.Second

// waitForOwnEvent waits until the sync API has the event, if the roomserver
// has accepted it and the requester sent it, and reports whether it arrived.
// Other events are not waited for, so a request for an event the server does
// not have, or will never pass to the sync API, is answered at once.
func waitForOwnEvent(
	ctx context.Context, syncDB storage.Database, rsAPI api.SyncRoomserverAPI,
	device *userapi.Device, roomID spec.RoomID, eventID string,
) bool {
	res := &api.QueryEventsByIDResponse{}
	if err := rsAPI.QueryEventsByID(ctx, &api.QueryEventsByIDRequest{
		RoomID: roomID.String(), EventIDs: []string{eventID}, ExcludeRejected: true,
	}, res); err != nil || len(res.Events) != 1 {
		return false
	}
	sender, err := rsAPI.QueryUserIDForSender(ctx, roomID, res.Events[0].SenderID())
	if err != nil || sender == nil || sender.String() != device.UserID {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, ownEventWait)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		snapshot, err := syncDB.NewDatabaseSnapshot(ctx)
		if err != nil {
			return false
		}
		events, err := snapshot.Events(ctx, []string{eventID})
		snapshot.Rollback() //nolint:errcheck
		if err == nil && len(events) > 0 {
			return true
		}
	}
}

// waitForRedaction waits until the sync API's copy of the event is redacted,
// if the roomserver's copy already is, and reports whether it was.
func waitForRedaction(
	ctx context.Context, syncDB storage.Database, rsAPI api.SyncRoomserverAPI,
	roomID spec.RoomID, eventID string,
) bool {
	res := &api.QueryEventsByIDResponse{}
	if err := rsAPI.QueryEventsByID(ctx, &api.QueryEventsByIDRequest{
		RoomID: roomID.String(), EventIDs: []string{eventID},
	}, res); err != nil || len(res.Events) != 1 || !isRedacted(res.Events[0].PDU) {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, ownEventWait)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		snapshot, err := syncDB.NewDatabaseSnapshot(ctx)
		if err != nil {
			return false
		}
		events, err := snapshot.Events(ctx, []string{eventID})
		snapshot.Rollback() //nolint:errcheck
		if err == nil && len(events) == 1 && isRedacted(events[0].PDU) {
			return true
		}
	}
}

// isRedacted reports whether the event has been redacted. Stored events carry
// the redaction in unsigned.redacted_because.
func isRedacted(ev gomatrixserverlib.PDU) bool {
	return ev.Redacted() || gjson.GetBytes(ev.JSON(), "unsigned.redacted_because").Exists()
}
