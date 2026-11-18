// Copyright 2024 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"net/http"
	"strings"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/clientapi/httputil"
	"codefloe.com/pat-s/zendrite/clientapi/producers"
	fedTypes "codefloe.com/pat-s/zendrite/federationapi/types"
	"codefloe.com/pat-s/zendrite/internal/eventutil"
	"codefloe.com/pat-s/zendrite/internal/transactions"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// SendToDevice handles PUT /_matrix/client/r0/sendToDevice/{eventType}/{txnId}
// sends the device events to the syncapi & federationsender.
func SendToDevice(
	req *http.Request, cfg *config.ClientAPI, device *userapi.Device,
	syncProducer *producers.SyncAPIProducer,
	txnCache *transactions.Cache,
	eventType string, txnID *string,
) util.JSONResponse {
	if txnID != nil {
		if res, ok := txnCache.FetchTransaction(device, *txnID, req.URL); ok {
			return *res
		}
	}

	var httpReq struct {
		Messages map[string]map[string]json.RawMessage `json:"messages"`
	}
	resErr := httputil.UnmarshalJSONRequest(req, &httpReq)
	if resErr != nil {
		return *resErr
	}

	// Each message for a remote user goes to its server in an EDU of its
	// own, which may not be larger than 64 KiB (MSC4514). Check them all
	// before sending any.
	for userID, byUser := range httpReq.Messages {
		_, domain, err := gomatrixserverlib.SplitID('@', userID)
		if err != nil || cfg.Matrix.IsLocalServerName(domain) {
			continue
		}
		for deviceID, message := range byUser {
			if eventutil.EDUTooLarge(spec.MDirectToDevice, device.UserDomain(), gomatrixserverlib.ToDeviceMessage{
				Sender: device.UserID, Type: eventType, MessageID: strings.Repeat("x", fedTypes.ToDeviceMessageIDLength),
				Messages: map[string]map[string]json.RawMessage{userID: {deviceID: message}},
			}) {
				return eventutil.EDUTooLargeResponse("A to-device message")
			}
		}
	}

	for userID, byUser := range httpReq.Messages {
		for deviceID, message := range byUser {
			if err := syncProducer.SendToDevice(
				req.Context(), device.UserID, userID, deviceID, eventType, message,
			); err != nil {
				util.GetLogger(req.Context()).WithError(err).Error("eduProducer.SendToDevice failed")
				return util.JSONResponse{
					Code: http.StatusInternalServerError,
					JSON: spec.InternalServerError{},
				}
			}
		}
	}

	res := util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct{}{},
	}

	if txnID != nil {
		txnCache.AddTransaction(device, *txnID, req.URL, &res)
	}

	return res
}
