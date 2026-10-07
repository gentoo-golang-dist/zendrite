// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// callGuestForbiddenEventTypes are the event types a call guest may not send,
// because they change who may join a room, who may do what in it, or what
// the room is. Element Call rooms set state_default to 0, so power levels
// alone would let a guest send any of them.
var callGuestForbiddenEventTypes = map[string]bool{
	spec.MRoomCreate:            true,
	spec.MRoomPowerLevels:       true,
	spec.MRoomJoinRules:         true,
	spec.MRoomHistoryVisibility: true,
	spec.MRoomGuestAccess:       true,
	spec.MRoomEncryption:        true,
	spec.MRoomCanonicalAlias:    true,
	spec.MRoomAliases:           true,
	spec.MRoomThirdPartyInvite:  true,
	spec.MSpaceChild:            true,
	spec.MSpaceParent:           true,
	"m.room.server_acl":         true,
	"m.room.tombstone":          true,
}

// callGuestEventForbidden refuses an event a call guest may not send: a
// structural event, or a membership event about another user, which would
// invite, kick or ban them. Other events, such as MatrixRTC call memberships,
// are left to the room's power levels. It returns nil for other accounts.
func callGuestEventForbidden(device *userapi.Device, eventType string, stateKey *string) *util.JSONResponse {
	if device == nil || device.AccountType != userapi.AccountTypeCallGuest {
		return nil
	}
	if callGuestForbiddenEventTypes[eventType] {
		return &util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.Forbidden("Call guest accounts may not send " + eventType + " events"),
		}
	}
	if eventType == spec.MRoomMember && (stateKey == nil || *stateKey != device.UserID) {
		return &util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.Forbidden("Call guest accounts may only change their own membership"),
		}
	}
	return nil
}
