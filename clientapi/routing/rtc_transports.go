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

// The unstable /org.matrix.msc4143/rtc/transports names the list
// rtc_transports, while the stable GET /v1/rtc/transports names it transports.
const (
	unstableRTCTransportsKey = "rtc_transports"
	stableRTCTransportsKey   = "transports"
)

// GetRTCTransports returns the MatrixRTC transports (MSC4143) under key. An
// unset list is returned as [] rather than null, so that, like Synapse, the
// stable endpoint answers without configured transports too.
func GetRTCTransports(rtcFoci []config.RTCFocus, key string) util.JSONResponse {
	if rtcFoci == nil {
		rtcFoci = []config.RTCFocus{}
	}
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: map[string][]config.RTCFocus{key: rtcFoci},
	}
}
