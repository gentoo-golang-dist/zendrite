// Copyright 2024 New Vector Ltd.
// Copyright 2017 Vector Creations Ltd
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/clientapi/httputil"
	"codefloe.com/pat-s/zendrite/clientapi/producers"
	"codefloe.com/pat-s/zendrite/internal/eventutil"
	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// GetAccountData implements GET /user/{userId}/[rooms/{roomid}/]account_data/{type}.
// With msc3391, entries with empty content count as deleted, as in Synapse.
func GetAccountData(
	req *http.Request, userAPI api.ClientUserAPI, device *api.Device,
	userID string, roomID string, dataType string, msc3391 bool,
) util.JSONResponse {
	if userID != device.UserID {
		return util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.Forbidden("userID does not match the current user"),
		}
	}

	dataReq := api.QueryAccountDataRequest{
		UserID:   userID,
		DataType: dataType,
		RoomID:   roomID,
	}
	dataRes := api.QueryAccountDataResponse{}
	if err := userAPI.QueryAccountData(req.Context(), &dataReq, &dataRes); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("userAPI.QueryAccountData failed")
		return util.ErrorResponse(fmt.Errorf("userAPI.QueryAccountData: %w", err))
	}

	var data json.RawMessage
	var ok bool
	if roomID != "" {
		data, ok = dataRes.RoomAccountData[roomID][dataType]
	} else {
		data, ok = dataRes.GlobalAccountData[dataType]
	}
	if ok && (!msc3391 || !isEmptyJSONObject(data)) {
		return util.JSONResponse{
			Code: http.StatusOK,
			JSON: data,
		}
	}

	return util.JSONResponse{
		Code: http.StatusNotFound,
		JSON: spec.NotFound("data not found"),
	}
}

// SaveAccountData implements PUT /user/{userId}/[rooms/{roomId}/]account_data/{type}.
// With msc3391, empty content ({}) deletes the entry, as in Synapse.
func SaveAccountData(
	req *http.Request, userAPI api.ClientUserAPI, device *api.Device,
	userID string, roomID string, dataType string, syncProducer *producers.SyncAPIProducer,
	msc3391 bool,
) util.JSONResponse {
	if userID != device.UserID {
		return util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.Forbidden("userID does not match the current user"),
		}
	}

	defer req.Body.Close()

	if req.Body == http.NoBody {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.NotJSON("Content not JSON"),
		}
	}

	if resErr := checkSettableAccountDataType(dataType); resErr != nil {
		return *resErr
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("io.ReadAll failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	if !json.Valid(body) {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("Bad JSON content"),
		}
	}

	if msc3391 && isEmptyJSONObject(body) {
		return removeAccountData(req, userAPI, userID, roomID, dataType)
	}

	dataReq := api.InputAccountDataRequest{
		UserID:      userID,
		DataType:    dataType,
		RoomID:      roomID,
		AccountData: json.RawMessage(body),
	}
	dataRes := api.InputAccountDataResponse{}
	if err := userAPI.InputAccountData(req.Context(), &dataReq, &dataRes); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("userAPI.InputAccountData failed")
		return util.ErrorResponse(err)
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct{}{},
	}
}

// DeleteAccountData implements MSC3391's
// DELETE /unstable/org.matrix.msc3391/user/{userId}/[rooms/{roomId}/]account_data/{type}.
// Deleting an entry that does not exist succeeds, as in Synapse.
func DeleteAccountData(
	req *http.Request, userAPI api.ClientUserAPI, device *api.Device,
	userID string, roomID string, dataType string,
) util.JSONResponse {
	if userID != device.UserID {
		return util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.Forbidden("Cannot delete account data for other users."),
		}
	}
	if resErr := checkSettableAccountDataType(dataType); resErr != nil {
		return *resErr
	}
	return removeAccountData(req, userAPI, userID, roomID, dataType)
}

// checkSettableAccountDataType refuses the account data types that clients
// may not set or delete through the account data endpoints.
func checkSettableAccountDataType(dataType string) *util.JSONResponse {
	// The spec reserves these types for the server: a 405 with M_BAD_JSON
	// rejects them for both global and room account data (MSC4010).
	if dataType == "m.fully_read" || dataType == "m.push_rules" {
		return &util.JSONResponse{
			Code: http.StatusMethodNotAllowed,
			JSON: spec.BadJSON(fmt.Sprintf("Cannot set %s through this API.", dataType)),
		}
	}
	if strings.HasPrefix(dataType, threePIDBindingPrefix) {
		return &util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.Forbidden(fmt.Sprintf("Unable to modify %q using this API", dataType)),
		}
	}
	return nil
}

func removeAccountData(
	req *http.Request, userAPI api.ClientUserAPI, userID, roomID, dataType string,
) util.JSONResponse {
	removeReq := api.PerformAccountDataRemovalRequest{
		UserID:   userID,
		RoomID:   roomID,
		DataType: dataType,
	}
	if err := userAPI.PerformAccountDataRemoval(req.Context(), &removeReq, &api.PerformAccountDataRemovalResponse{}); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("userAPI.PerformAccountDataRemoval failed")
		return util.ErrorResponse(err)
	}
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct{}{},
	}
}

// isEmptyJSONObject reports whether data is a JSON object without keys.
func isEmptyJSONObject(data []byte) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(data, &obj) == nil && obj != nil && len(obj) == 0
}

type fullyReadEvent struct {
	EventID string `json:"event_id"`
}

// SaveReadMarker implements POST /rooms/{roomId}/read_markers.
func SaveReadMarker(
	req *http.Request,
	userAPI api.ClientUserAPI, rsAPI roomserverAPI.ClientRoomserverAPI,
	syncProducer *producers.SyncAPIProducer, device *api.Device, roomID string,
) util.JSONResponse {
	deviceUserID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("userID for this device is invalid"),
		}
	}

	// Verify that the user is a member of this room
	resErr := checkMemberInRoom(req.Context(), rsAPI, *deviceUserID, roomID)
	if resErr != nil {
		return *resErr
	}

	var r struct {
		eventutil.ReadMarkerJSON
		AllowBackward json.RawMessage `json:"com.beeper.allow_backward"`
	}
	resErr = httputil.UnmarshalJSONRequest(req, &r)
	if resErr != nil {
		return *resErr
	}
	// MSC4446: com.beeper.allow_backward lets m.fully_read move backwards.
	// Read receipts are not affected by it.
	allowBackward, resErr := parseAllowBackward(r.AllowBackward)
	if resErr != nil {
		return *resErr
	}

	if r.FullyRead != "" {
		if resErr = setFullyRead(req.Context(), rsAPI, userAPI, device.UserID, roomID, r.FullyRead, allowBackward); resErr != nil {
			return *resErr
		}
	}

	// Handle the read receipts that may be included in the read marker.
	for _, receipt := range [][2]string{{"m.read", r.Read}, {"m.read.private", r.ReadPrivate}} {
		if receipt[1] == "" {
			continue
		}
		if res := setReceipt(req, syncProducer, device, roomID, receipt[0], receipt[1], ""); res.Code != http.StatusOK {
			return res
		}
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct{}{},
	}
}
