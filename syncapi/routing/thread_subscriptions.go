// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/tidwall/gjson"

	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/syncapi/internal"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// The MSC4306 error codes, with their unstable names.
const (
	errNotInThread             = "IO.ELEMENT.MSC4306.M_NOT_IN_THREAD"
	errConflictingUnsubscribed = "IO.ELEMENT.MSC4306.M_CONFLICTING_UNSUBSCRIPTION"
)

// ThreadSubscriptionNotifier is told about each change to a user's thread
// subscriptions, so that it can wake the user's sync requests.
type ThreadSubscriptionNotifier func(userID string, pos int64)

// threadSubscriptionRequest holds what the MSC4306 endpoints share.
type threadSubscriptionRequest struct {
	ctx          context.Context
	db           storage.DatabaseTransaction
	rsAPI        rsapi.SyncRoomserverAPI
	userID       spec.UserID
	roomID       string
	threadRootID string
}

// ThreadSubscription implements the MSC4306 endpoints
//
//	GET|PUT|DELETE /_matrix/client/unstable/io.element.msc4306/rooms/{roomId}/thread/{eventId}/subscription
//
// as Synapse does. The thread root must be an event the user may see;
// otherwise, and for unknown events, the answer is 404.
func ThreadSubscription(
	req *http.Request, device *userapi.Device, roomID, threadRootID string,
	syncDB storage.Database, rsAPI rsapi.SyncRoomserverAPI, userAPI userapi.SyncUserAPI,
	notify ThreadSubscriptionNotifier,
) util.JSONResponse {
	if _, err := spec.NewRoomID(roomID); err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("invalid room ID")}
	}
	if !strings.HasPrefix(threadRootID, "$") {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Invalid event ID")}
	}
	userID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		return util.ErrorResponse(err)
	}

	var body struct {
		Automatic *string `json:"automatic"`
	}
	if req.Method == http.MethodPut {
		if resErr := parseThreadSubscriptionBody(req, &body.Automatic); resErr != nil {
			return *resErr
		}
	}

	ctx := req.Context()
	snapshot, err := syncDB.NewDatabaseSnapshot(ctx)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("ThreadSubscription: NewDatabaseSnapshot failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	defer snapshot.Rollback() //nolint:errcheck

	r := &threadSubscriptionRequest{ctx: ctx, db: snapshot, rsAPI: rsAPI, userID: *userID, roomID: roomID, threadRootID: threadRootID}
	root, err := r.visibleEvent(threadRootID)
	if err != nil {
		return internalError(ctx, err, "ThreadSubscription: loading the thread root failed")
	}
	if root == nil {
		return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("No such thread root")}
	}

	switch req.Method {
	case http.MethodGet:
		subscription, err := userAPI.QueryThreadSubscription(ctx, device.UserID, roomID, threadRootID)
		if err != nil {
			return internalError(ctx, err, "ThreadSubscription: QueryThreadSubscription failed")
		}
		if subscription == nil {
			return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Not subscribed.")}
		}
		return util.JSONResponse{Code: http.StatusOK, JSON: map[string]bool{"automatic": subscription.Automatic}}

	case http.MethodPut:
		return r.subscribe(body.Automatic, userAPI, notify)

	default:
		return r.unsubscribe(userAPI, notify)
	}
}

// parseThreadSubscriptionBody reads the PUT body, whose optional automatic
// field is the event ID that caused an automatic subscription.
func parseThreadSubscriptionBody(req *http.Request, automatic **string) *util.JSONResponse {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("unable to read the request body")}
	}
	var body map[string]json.RawMessage
	if err = json.Unmarshal(raw, &body); err != nil || body == nil {
		return &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("the request body must be a JSON object")}
	}
	value, ok := body["automatic"]
	if !ok || string(value) == "null" {
		return nil
	}
	var eventID string
	if err = json.Unmarshal(value, &eventID); err != nil || !strings.HasPrefix(eventID, "$") {
		return &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("automatic must be an event ID")}
	}
	*automatic = &eventID
	return nil
}

// visibleEvent returns an event of the room that the user may see, or nil.
func (r *threadSubscriptionRequest) visibleEvent(eventID string) (*rstypes.HeaderedEvent, error) {
	events, err := r.db.Events(r.ctx, []string{eventID})
	if err != nil {
		return nil, err
	}
	if len(events) != 1 || events[0].RoomID().String() != r.roomID {
		return nil, nil
	}
	events, err = internal.ApplyHistoryVisibilityFilter(r.ctx, r.db, r.rsAPI, events, nil, r.userID, "thread_subscription")
	if err != nil || len(events) != 1 {
		return nil, err
	}
	return events[0], nil
}

func (r *threadSubscriptionRequest) subscribe(automatic *string, userAPI userapi.SyncUserAPI, notify ThreadSubscriptionNotifier) util.JSONResponse {
	subscribeReq := &userapi.PerformThreadSubscribeRequest{
		UserID:       r.userID.String(),
		RoomID:       r.roomID,
		ThreadRootID: r.threadRootID,
	}
	if automatic != nil {
		cause, err := r.visibleEvent(*automatic)
		if err != nil {
			return internalError(r.ctx, err, "ThreadSubscription: loading the cause event failed")
		}
		if cause == nil {
			return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Automatic subscription event not found")}
		}
		relation := gjson.GetBytes(cause.Content(), `m\.relates_to`)
		if relation.Get("rel_type").Str != "m.thread" || relation.Get("event_id").Str != r.threadRootID {
			return util.MatrixErrorResponse(http.StatusBadRequest, errNotInThread, "Automatic subscription must use an event in the thread")
		}
		_, causePos, err := r.db.PositionInTopology(r.ctx, cause.EventID())
		if err != nil {
			return internalError(r.ctx, err, "ThreadSubscription: PositionInTopology failed")
		}
		subscribeReq.Automatic = true
		subscribeReq.CauseStreamPos = int64(causePos)
	}
	res, err := userAPI.PerformThreadSubscribe(r.ctx, subscribeReq)
	if err != nil {
		return internalError(r.ctx, err, "ThreadSubscription: PerformThreadSubscribe failed")
	}
	if res.Conflict {
		return util.MatrixErrorResponse(http.StatusConflict, errConflictingUnsubscribed, "Automatic subscription obsoleted by an unsubscription request.")
	}
	if res.StreamID > 0 && notify != nil {
		notify(r.userID.String(), res.StreamID)
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

func (r *threadSubscriptionRequest) unsubscribe(userAPI userapi.SyncUserAPI, notify ThreadSubscriptionNotifier) util.JSONResponse {
	// Automatic subscriptions caused by the events seen so far must not
	// undo this unsubscription.
	positions, err := r.db.MaxStreamPositionsForRooms(r.ctx, []string{r.roomID})
	if err != nil {
		return internalError(r.ctx, err, "ThreadSubscription: MaxStreamPositionsForRooms failed")
	}
	streamID, err := userAPI.PerformThreadUnsubscribe(r.ctx, &userapi.PerformThreadUnsubscribeRequest{
		UserID:        r.userID.String(),
		RoomID:        r.roomID,
		ThreadRootID:  r.threadRootID,
		RoomStreamPos: int64(positions[r.roomID]),
	})
	if err != nil {
		return internalError(r.ctx, err, "ThreadSubscription: PerformThreadUnsubscribe failed")
	}
	if streamID > 0 && notify != nil {
		notify(r.userID.String(), streamID)
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

// threadSubscriptionsPage is a page of the MSC4308 endpoint.
type threadSubscriptionsPage struct {
	types.ThreadSubscriptionsResponse
	// End is the from token of the next page, if there may be one.
	End string `json:"end,omitempty"`
}

// ThreadSubscriptions implements the MSC4308 companion endpoint
//
//	GET /_matrix/client/unstable/io.element.msc4308/thread_subscriptions
//
// which pages backwards through the latest changes to the user's thread
// subscriptions, as Synapse does. The from parameter is a token from a
// previous page or the extension's prev_batch, and to is a thread
// subscriptions token or a sliding sync pos.
func ThreadSubscriptions(req *http.Request, device *userapi.Device, userAPI userapi.SyncUserAPI) util.JSONResponse {
	ctx := req.Context()
	query := req.URL.Query()
	if !query.Has("dir") {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("dir is required")}
	}
	if query.Get("dir") != "b" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("dir must be b")}
	}
	limit := types.DefaultThreadSubscriptionsLimit
	if query.Has("limit") {
		var err error
		if limit, err = strconv.Atoi(query.Get("limit")); err != nil || limit <= 0 {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("limit must be greater than 0")}
		}
	}
	limit = min(limit, types.MaxThreadSubscriptionsLimit)

	// Paging backwards, from is the later bound (inclusive) and to the
	// earlier one (exclusive).
	var end int64
	if from := query.Get("from"); from != "" {
		var err error
		if end, err = types.ParseThreadSubscriptionsToken(from); err != nil {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("`from` is not a valid token")}
		}
	} else {
		var err error
		if end, err = userAPI.QueryMaxThreadSubscriptionStreamID(ctx); err != nil {
			return internalError(ctx, err, "ThreadSubscriptions: QueryMaxThreadSubscriptionStreamID failed")
		}
	}
	var start int64
	if to := query.Get("to"); to != "" {
		var err error
		if start, err = types.ParseThreadSubscriptionsToken(to); err != nil {
			pos, posErr := types.ParseSlidingSyncStreamToken(to)
			if posErr != nil || pos == nil {
				return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("`to` is not a valid token")}
			}
			start = int64(pos.StreamToken.ThreadSubscriptionPosition)
		}
	}

	var updates []userapi.ThreadSubscriptionUpdate
	if end > start {
		var err error
		if updates, err = userAPI.QueryThreadSubscriptionUpdates(ctx, device.UserID, start, end, limit); err != nil {
			return internalError(ctx, err, "ThreadSubscriptions: QueryThreadSubscriptionUpdates failed")
		}
	}
	page := threadSubscriptionsPage{}
	page.ThreadSubscriptionsResponse, page.End = types.NewThreadSubscriptionsResponse(updates, limit)
	return util.JSONResponse{Code: http.StatusOK, JSON: page}
}

func internalError(ctx context.Context, err error, msg string) util.JSONResponse {
	util.GetLogger(ctx).WithError(err).Error(msg)
	return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
}
