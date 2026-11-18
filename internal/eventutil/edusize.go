// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package eventutil

import (
	"encoding/json"
	"fmt"
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

// MaxEDUSize is the largest an EDU may be in canonical JSON (MSC4514), as
// for events.
const MaxEDUSize = 65536

// eduOverhead bounds the JSON around an EDU's content, type and origin:
// keys, quotes, braces and commas.
const eduOverhead = 64

// EDUSize returns the size of an EDU in canonical JSON, as it is sent in a
// transaction, with its origin.
func EDUSize(eduType string, origin spec.ServerName, content any) (int, error) {
	raw, err := json.Marshal(struct {
		Type    string `json:"edu_type"`
		Origin  string `json:"origin"`
		Content any    `json:"content"`
	}{eduType, string(origin), content})
	if err != nil {
		return 0, err
	}
	// Measure leniently: this may be stored JSON from before the MSC4458
	// checks, and new input has passed them already.
	return len(gomatrixserverlib.CanonicalJSONAssumeValid(raw)), nil
}

// EDUTooLarge reports whether an EDU would be larger than MaxEDUSize. Content
// that cannot be encoded is not judged here: whatever handles the EDU
// rejects it for what it is.
func EDUTooLarge(eduType string, origin spec.ServerName, content any) bool {
	// Canonical JSON is never longer than the compact JSON it comes from, so
	// small raw content needs no encoding.
	if raw, ok := content.(spec.RawJSON); ok && len(raw)+len(eduType)+len(origin)+eduOverhead <= MaxEDUSize {
		return false
	}
	size, err := EDUSize(eduType, origin, content)
	return err == nil && size > MaxEDUSize
}

// EDUTooLargeResponse is the answer to a client request whose EDU would be
// larger than MaxEDUSize (MSC4514).
func EDUTooLargeResponse(what string) util.JSONResponse {
	return util.JSONResponse{
		Code: http.StatusRequestEntityTooLarge,
		JSON: spec.TooLarge(fmt.Sprintf("%s would make an EDU larger than %d bytes.", what, MaxEDUSize)),
	}
}
