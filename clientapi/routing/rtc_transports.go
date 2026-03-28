// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"net/http"

	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// RTCTransport represents a transport in the response.
type RTCTransport struct {
	Type              string `json:"type"`
	LiveKitServiceURL string `json:"livekit_service_url,omitempty"`
}

// RTCTransportsResponse represents the response for /rtc/transports.
type RTCTransportsResponse struct {
	RTCTransports []RTCTransport `json:"rtc_transports"`
}

// GetRTCTransports returns the list of MatrixRTC transports (MSC4143).
func GetRTCTransports(rtcFoci []config.RTCFocus) util.JSONResponse {
	transports := make([]RTCTransport, 0, len(rtcFoci))

	for _, focus := range rtcFoci {
		transport := RTCTransport{
			Type: focus.Type,
		}
		if focus.LiveKit != nil {
			transport.LiveKitServiceURL = focus.LiveKit.ServiceURL
		}
		transports = append(transports, transport)
	}

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: RTCTransportsResponse{
			RTCTransports: transports,
		},
	}
}
