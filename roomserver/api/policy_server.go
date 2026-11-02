// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package api

import (
	"net/http"
	"strings"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

// Event types that name a room's policy server (MSC4284, spec v1.18).
const (
	MRoomPolicy         = "m.room.policy"
	UnstableMRoomPolicy = "org.matrix.msc4284.policy"
)

// Markers that the roomserver puts in front of the reason when it refuses a
// local event because of the room's policy server (MSC4284). They let the
// client API answer with the policy server's error rather than a 500.
const (
	policyServerRefusedMarker     = "PolicyServerRefused"
	policyServerUnavailableMarker = "PolicyServerUnavailable"
)

// PolicyServerRefusedReason is the rejection reason for a local event that
// the policy server refused to sign, with the error it answered.
func PolicyServerRefusedReason(errCode, message string) string {
	if errCode == "" {
		errCode = string(spec.ErrorForbidden)
	}
	if message == "" {
		message = "The room's policy server refused the event"
	}
	return policyServerRefusedMarker + ":" + errCode + ":" + message
}

// PolicyServerUnavailableReason is the rejection reason for a local event
// that could not be checked because the policy server could not be asked.
func PolicyServerUnavailableReason() string {
	return policyServerUnavailableMarker + ":The room's policy server could not be reached"
}

// PolicyServerRejection turns an input error for a local event refused
// because of the room's policy server into the response for the client: 400
// with the policy server's error when it refused, and 503 when it could not be
// reached. It returns false for any other error.
//
// The perform APIs pass the rejection reason on without the InputWasRejected
// marker, so a bare reason is recognized too.
func PolicyServerRejection(err error) (util.JSONResponse, bool) {
	if err == nil {
		return util.JSONResponse{}, false
	}
	reason := InputRejectionReason(err)
	if rest, ok := strings.CutPrefix(reason, policyServerRefusedMarker+":"); ok {
		errCode, message, _ := strings.Cut(rest, ":")
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.MatrixError{ErrCode: spec.MatrixErrorCode(errCode), Err: message},
		}, true
	}
	if message, ok := strings.CutPrefix(reason, policyServerUnavailableMarker+":"); ok {
		return util.JSONResponse{
			Code: http.StatusServiceUnavailable,
			JSON: spec.MatrixError{ErrCode: spec.ErrorUnknown, Err: message},
		}, true
	}
	return util.JSONResponse{}, false
}
