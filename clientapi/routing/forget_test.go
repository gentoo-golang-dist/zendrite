// Copyright 2025 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.
package routing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"

	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/test"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// forgetTestRoomserverAPI is a minimal fake that lets us drive SendForget with
// a membership table (IsInRoom) that disagrees with current room state.
type forgetTestRoomserverAPI struct {
	rsapi.ClientRoomserverAPI

	isInRoom       bool
	senderID       spec.SenderID
	memberEvent    *rstypes.HeaderedEvent
	forgetExecuted bool
}

func (r *forgetTestRoomserverAPI) QueryMembershipForUser(
	ctx context.Context, req *rsapi.QueryMembershipForUserRequest, res *rsapi.QueryMembershipForUserResponse,
) error {
	res.RoomExists = true
	res.IsInRoom = r.isInRoom
	sid := r.senderID
	res.SenderID = &sid
	return nil
}

func (r *forgetTestRoomserverAPI) QueryCurrentState(
	ctx context.Context, req *rsapi.QueryCurrentStateRequest, res *rsapi.QueryCurrentStateResponse,
) error {
	res.StateEvents = map[gomatrixserverlib.StateKeyTuple]*rstypes.HeaderedEvent{}
	if r.memberEvent == nil {
		return nil
	}
	for _, tuple := range req.StateTuples {
		if tuple.EventType == spec.MRoomMember && tuple.StateKey == string(r.senderID) {
			res.StateEvents[tuple] = r.memberEvent
		}
	}
	return nil
}

func (r *forgetTestRoomserverAPI) PerformForget(
	ctx context.Context, req *rsapi.PerformForgetRequest, res *rsapi.PerformForgetResponse,
) error {
	r.forgetExecuted = true
	return nil
}

// TestSendForgetStaleMembershipRow guards the fix for the case where the cached
// membership row is stale at "join" (e.g. after a remote kick of the last local
// member) while current state already records the leave. /forget must defer to
// current state and succeed rather than refusing with "still in room".
func TestSendForgetStaleMembershipRow(t *testing.T) {
	user := test.NewUser(t)
	device := &userapi.Device{UserID: user.ID}

	makeRequest := func() *http.Request {
		return httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/rooms/!r:test/forget", nil)
	}

	t.Run("kicked user can forget despite stale join row", func(t *testing.T) {
		room := test.NewRoom(t, user)
		leaveEvent := room.CreateEvent(t, user, spec.MRoomMember, map[string]any{
			"membership": "leave",
		}, test.WithStateKey(user.ID))

		rsAPI := &forgetTestRoomserverAPI{
			isInRoom:    true, // stale membership table
			senderID:    spec.SenderID(user.ID),
			memberEvent: leaveEvent,
		}

		resp := SendForget(makeRequest(), device, room.ID, rsAPI)
		if resp.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (%+v)", resp.Code, resp.JSON)
		}
		if !rsAPI.forgetExecuted {
			t.Fatal("expected PerformForget to be called")
		}
	})

	t.Run("genuinely joined user is still refused", func(t *testing.T) {
		room := test.NewRoom(t, user)
		joinEvent := room.CreateEvent(t, user, spec.MRoomMember, map[string]any{
			"membership": "join",
		}, test.WithStateKey(user.ID))

		rsAPI := &forgetTestRoomserverAPI{
			isInRoom:    true,
			senderID:    spec.SenderID(user.ID),
			memberEvent: joinEvent,
		}

		resp := SendForget(makeRequest(), device, room.ID, rsAPI)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d (%+v)", resp.Code, resp.JSON)
		}
		if rsAPI.forgetExecuted {
			t.Fatal("expected PerformForget NOT to be called for a joined user")
		}
	})
}
