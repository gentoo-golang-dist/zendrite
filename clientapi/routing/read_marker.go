// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// allowBackwardField is the unstable request body field of MSC4446.
const allowBackwardField = "com.beeper.allow_backward"

// EventOrderingQuerier finds the positions of events in a room.
type EventOrderingQuerier interface {
	QueryEventOrdering(ctx context.Context, roomID string, eventIDs []string) (map[string]roomserverAPI.EventOrdering, error)
}

// parseAllowBackward reads MSC4446's com.beeper.allow_backward from a
// request body. It is false when absent and must otherwise be a boolean.
func parseAllowBackward(raw json.RawMessage) (bool, *util.JSONResponse) {
	if raw == nil {
		return false, nil
	}
	var allowBackward *bool
	if err := json.Unmarshal(raw, &allowBackward); err != nil || allowBackward == nil {
		return false, &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam(allowBackwardField + " must be a boolean."),
		}
	}
	return *allowBackward, nil
}

// fullyReadLocks serializes m.fully_read updates per user and room, like
// Synapse's read_marker linearizer, so that two concurrent forward-only
// updates cannot both pass the ordering check against the same old marker.
var fullyReadLocks = keyedMutex{locks: map[[2]string]*keyedMutexEntry{}}

type keyedMutexEntry struct {
	mu   sync.Mutex
	refs int
}

type keyedMutex struct {
	mu    sync.Mutex
	locks map[[2]string]*keyedMutexEntry
}

func (k *keyedMutex) lock(key [2]string) func() {
	k.mu.Lock()
	entry := k.locks[key]
	if entry == nil {
		entry = &keyedMutexEntry{}
		k.locks[key] = entry
	}
	entry.refs++
	k.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		k.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// setFullyRead stores eventID as the user's m.fully_read marker in the room.
// Unless allowBackward is set (MSC4446), the marker only moves forward: an
// event at or before the current marker leaves the marker as it is. The
// event must be known in the room. A current marker whose event is no longer
// known is treated as older than any event. This follows Synapse's
// ReadMarkerHandler.received_client_read_marker.
func setFullyRead(
	ctx context.Context, rsAPI EventOrderingQuerier, userAPI userapi.ClientUserAPI,
	userID, roomID, eventID string, allowBackward bool,
) *util.JSONResponse {
	unlock := fullyReadLocks.lock([2]string{userID, roomID})
	defer unlock()

	var existing string
	if !allowBackward {
		var res userapi.QueryAccountDataResponse
		if err := userAPI.QueryAccountData(ctx, &userapi.QueryAccountDataRequest{
			UserID: userID, RoomID: roomID, DataType: "m.fully_read",
		}, &res); err != nil {
			util.GetLogger(ctx).WithError(err).Error("userAPI.QueryAccountData failed")
			return &util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
		var marker fullyReadEvent
		if data := res.RoomAccountData[roomID]["m.fully_read"]; len(data) > 0 && json.Unmarshal(data, &marker) == nil {
			existing = marker.EventID
		}
	}

	eventIDs := []string{eventID}
	if existing != "" && existing != eventID {
		eventIDs = append(eventIDs, existing)
	}
	orderings, err := rsAPI.QueryEventOrdering(ctx, roomID, eventIDs)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("rsAPI.QueryEventOrdering failed")
		return &util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	ordering, ok := orderings[eventID]
	if !ok {
		return &util.JSONResponse{
			Code: http.StatusNotFound,
			JSON: spec.Unknown(fmt.Sprintf("Could not find event %s in room %s", eventID, roomID)),
		}
	}
	if existing == eventID {
		return nil
	}
	if old, known := orderings[existing]; existing != "" && known && !old.Before(ordering) {
		return nil
	}

	data, err := json.Marshal(fullyReadEvent{EventID: eventID})
	if err != nil {
		return &util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if err := userAPI.InputAccountData(ctx, &userapi.InputAccountDataRequest{
		UserID: userID, DataType: "m.fully_read", RoomID: roomID, AccountData: data,
	}, &userapi.InputAccountDataResponse{}); err != nil {
		util.GetLogger(ctx).WithError(err).Error("userAPI.InputAccountData failed")
		jsonErr := util.ErrorResponse(err)
		return &jsonErr
	}
	return nil
}
