// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/clientapi/producers"
	"codefloe.com/pat-s/zendrite/internal/receipts"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// ReceiptQuerier is what SetReceipt needs from the roomserver.
type ReceiptQuerier interface {
	receipts.EventQuerier
	EventOrderingQuerier
}

func SetReceipt(req *http.Request, rsAPI ReceiptQuerier, userAPI userapi.ClientUserAPI, syncProducer *producers.SyncAPIProducer, device *userapi.Device, roomID, receiptType, eventID string) util.JSONResponse {
	var body map[string]json.RawMessage
	if req.Body != nil {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return util.ErrorResponse(err)
		}
		if len(data) > 0 {
			if err = json.Unmarshal(data, &body); err != nil || body == nil {
				return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("Expected a JSON object")}
			}
		}
	}
	// MSC4446: only m.fully_read may move backwards.
	allowBackward, errRes := parseAllowBackward(body[allowBackwardField])
	if errRes != nil {
		return *errRes
	}
	if allowBackward && receiptType != "m.fully_read" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam(allowBackwardField + " is only allowed to be true for m.fully_read.")}
	}
	var threadID string
	if raw, present := body["thread_id"]; present {
		if err := json.Unmarshal(raw, &threadID); err != nil || threadID == "" || (threadID != "main" && (!strings.HasPrefix(threadID, "$") || len(threadID) < 2)) {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("thread_id must be main or a thread root event ID")}
		}
		if receiptType != "m.read" && receiptType != "m.read.private" {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("thread_id is incompatible with this receipt type")}
		}
		related, err := receipts.RelatedToThread(req.Context(), rsAPI, roomID, eventID, threadID)
		if errors.Is(err, receipts.ErrUnknownEvent) || (err == nil && !related) {
			return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("event_id is not related to thread_id")}
		}
		if err != nil {
			return util.ErrorResponse(err)
		}
	}
	if receiptType == "m.fully_read" {
		if errRes := setFullyRead(req.Context(), rsAPI, userAPI, device.UserID, roomID, eventID, allowBackward); errRes != nil {
			return *errRes
		}
		return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
	}
	return setReceipt(req, syncProducer, device, roomID, receiptType, eventID, threadID)
}

func setReceipt(req *http.Request, syncProducer *producers.SyncAPIProducer, device *userapi.Device, roomID, receiptType, eventID, threadID string) util.JSONResponse {
	timestamp := spec.AsTimestamp(time.Now())
	logrus.WithFields(logrus.Fields{
		"roomID":      roomID,
		"receiptType": receiptType,
		"eventID":     eventID,
		"userId":      device.UserID,
		"timestamp":   timestamp,
	}).Debug("Setting receipt")

	switch receiptType {
	case "m.read", "m.read.private":
		if err := syncProducer.SendReceipt(req.Context(), device.UserID, roomID, eventID, receiptType, threadID, timestamp); err != nil {
			return util.ErrorResponse(err)
		}

	default:
		return util.MessageResponse(400, fmt.Sprintf("Receipt type '%s' not known", receiptType)) //nolint:mnd
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct{}{},
	}
}
