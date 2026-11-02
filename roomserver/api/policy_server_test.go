// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package api

import (
	"errors"
	"net/http"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
)

func TestPolicyServerRejection(t *testing.T) {
	refused := PolicyServerRefusedReason("M_FORBIDDEN", "Too many mentions: 12")
	for _, tc := range []struct {
		name    string
		err     error
		code    int
		errCode spec.MatrixErrorCode
		message string
	}{
		{"refused through input", errors.New(InputWasRejected + ": " + refused), http.StatusBadRequest, "M_FORBIDDEN", "Too many mentions: 12"},
		{"refused, bare reason", errors.New(refused), http.StatusBadRequest, "M_FORBIDDEN", "Too many mentions: 12"},
		{"refused without details", errors.New(PolicyServerRefusedReason("", "")), http.StatusBadRequest, "M_FORBIDDEN", "The room's policy server refused the event"},
		{"unavailable", errors.New(InputWasRejected + ": " + PolicyServerUnavailableReason()), http.StatusServiceUnavailable, spec.ErrorUnknown, "The room's policy server could not be reached"},
		{"other rejection", errors.New(InputWasRejected + ": not allowed"), 0, "", ""},
		{"nil", nil, 0, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, ok := PolicyServerRejection(tc.err)
			if ok != (tc.code != 0) {
				t.Fatalf("got ok=%v", ok)
			}
			if !ok {
				return
			}
			got, isMatrixErr := res.JSON.(spec.MatrixError)
			if !isMatrixErr {
				t.Fatalf("got %T", res.JSON)
			}
			if res.Code != tc.code || got.ErrCode != tc.errCode || got.Err != tc.message {
				t.Fatalf("got %d %+v", res.Code, got)
			}
		})
	}
}
