// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

// malformedOwnedStateKey answers 400 M_BAD_JSON for a refused state event
// whose state key starts with '@' but neither equals a valid user ID nor
// starts with one plus an underscore, in a room version with MSC3757 owned
// state. Synapse answers such events the same way, and other refusals stay
// 403.
func malformedOwnedStateKey(event gomatrixserverlib.PDU) *util.JSONResponse {
	stateKey := event.StateKey()
	if stateKey == nil || len(*stateKey) == 0 || (*stateKey)[0] != '@' || spec.SenderID(*stateKey) == event.SenderID() {
		return nil
	}
	verImpl, err := gomatrixserverlib.GetRoomVersion(event.Version())
	if err != nil || !verImpl.OwnedState() {
		return nil
	}
	if _, err := gomatrixserverlib.OwnedStateKeyUserID(*stateKey); err != nil {
		return &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON(err.Error()),
		}
	}
	return nil
}
