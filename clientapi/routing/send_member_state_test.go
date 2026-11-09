// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	uapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// memberStateRoomserverAPI stands in for the roomserver: the user's membership
// is "before" until a join or leave is performed, and "after" from then on.
type memberStateRoomserverAPI struct {
	rsapi.ClientRoomserverAPI
	before, after rsapi.QueryMembershipForUserResponse
	changed       bool
	joins, leaves int
	// remoteLeave is the leave event a remote server took, if any.
	remoteLeave string
}

func (s *memberStateRoomserverAPI) QueryMembershipForUser(_ context.Context, _ *rsapi.QueryMembershipForUserRequest, res *rsapi.QueryMembershipForUserResponse) error {
	if s.changed {
		*res = s.after
	} else {
		*res = s.before
	}
	return nil
}

func (s *memberStateRoomserverAPI) QueryCurrentState(_ context.Context, _ *rsapi.QueryCurrentStateRequest, res *rsapi.QueryCurrentStateResponse) error {
	res.StateEvents = nil
	return nil
}

func (s *memberStateRoomserverAPI) PerformJoin(_ context.Context, req *rsapi.PerformJoinRequest) (string, spec.ServerName, error) {
	s.joins++
	s.changed = true
	return req.RoomIDOrAlias, "", nil
}

func (s *memberStateRoomserverAPI) PerformLeave(_ context.Context, _ *rsapi.PerformLeaveRequest, res *rsapi.PerformLeaveResponse) error {
	res.EventID = s.remoteLeave
	s.leaves++
	s.changed = true
	return nil
}

func TestSendMembershipStateEvent(t *testing.T) {
	const (
		roomID = "!room:remote.example"
		alice  = "@alice:test"
	)
	membership := func(m, eventID string) rsapi.QueryMembershipForUserResponse {
		return rsapi.QueryMembershipForUserResponse{Membership: m, EventID: eventID}
	}

	tests := []struct {
		name         string
		stateKey     string
		body         string
		before       rsapi.QueryMembershipForUserResponse
		after        rsapi.QueryMembershipForUserResponse
		remoteLeave  string
		wantHandled  bool
		wantLeaves   int
		wantEventID  string
		wantHTTPCode int
	}{
		{
			// A per-room nickname or avatar is a join sent by a joined user.
			// It must stay a plain event with the client's content.
			name:     "join while joined is left to the plain event path",
			stateKey: alice,
			body:     `{"membership":"join","displayname":"nick"}`,
			before:   membership(spec.Join, "$join"),
		},
		{
			name:     "another user's membership is left to the plain event path",
			stateKey: "@bob:test",
			body:     `{"membership":"leave"}`,
			before:   membership(spec.Join, "$join"),
		},
		{
			name:     "invite is left to sendMemberInvite",
			stateKey: alice,
			body:     `{"membership":"invite"}`,
			before:   membership(spec.Leave, "$leave"),
		},
		{
			name:     "ban is left to the plain event path",
			stateKey: alice,
			body:     `{"membership":"ban"}`,
			before:   membership(spec.Join, "$join"),
		},
		{
			// The leave goes through the remote server and this one only
			// forgets the membership, so the leave event is not stored here.
			name:         "rejecting an invite to a remote room answers with the remote leave event",
			stateKey:     alice,
			body:         `{"membership":"leave"}`,
			before:       membership(spec.Invite, "$invite"),
			remoteLeave:  "$remote_leave",
			wantHandled:  true,
			wantLeaves:   1,
			wantEventID:  "$remote_leave",
			wantHTTPCode: http.StatusOK,
		},
		{
			// No remote server took the leave, but the invite is retired.
			name:         "rejecting an invite no remote server took answers with the invite",
			stateKey:     alice,
			body:         `{"membership":"leave"}`,
			before:       membership(spec.Invite, "$invite"),
			wantHandled:  true,
			wantLeaves:   1,
			wantEventID:  "$invite",
			wantHTTPCode: http.StatusOK,
		},
		{
			name:         "withdrawing a knock on a remote room answers with the knock",
			stateKey:     alice,
			body:         `{"membership":"leave"}`,
			before:       membership(spec.Knock, "$knock"),
			wantHandled:  true,
			wantLeaves:   1,
			wantEventID:  "$knock",
			wantHTTPCode: http.StatusOK,
		},
		{
			name:         "leaving a room this server is in answers with the leave event",
			stateKey:     alice,
			body:         `{"membership":"leave"}`,
			before:       membership(spec.Join, "$join"),
			after:        membership(spec.Leave, "$leave"),
			wantHandled:  true,
			wantLeaves:   1,
			wantEventID:  "$leave",
			wantHTTPCode: http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := &memberStateRoomserverAPI{before: tc.before, after: tc.after, remoteLeave: tc.remoteLeave}
			req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(tc.body))
			device := &uapi.Device{UserID: alice}

			res, handled := SendMembershipStateEvent(req, device, roomID, spec.MRoomMember, tc.stateKey, rs, nil)
			if handled != tc.wantHandled {
				t.Fatalf("handled = %v, want %v (response %+v)", handled, tc.wantHandled, res)
			}
			if rs.leaves != tc.wantLeaves || rs.joins != 0 {
				t.Fatalf("performed %d leaves and %d joins, want %d leaves and none", rs.leaves, rs.joins, tc.wantLeaves)
			}
			if !handled {
				// The request must reach the plain event path unchanged.
				got, err := io.ReadAll(req.Body)
				if err != nil || string(got) != tc.body {
					t.Fatalf("body = %q (err %v), want it left readable as %q", got, err, tc.body)
				}
				return
			}
			if res.Code != tc.wantHTTPCode {
				t.Fatalf("code = %d, want %d (%+v)", res.Code, tc.wantHTTPCode, res.JSON)
			}
			got, ok := res.JSON.(sendEventResponse)
			if !ok || got.EventID != tc.wantEventID {
				t.Fatalf("response = %+v, want event_id %q", res.JSON, tc.wantEventID)
			}
		})
	}
}

// joinContentRoomserverAPI records the content of the join it is asked for.
type joinContentRoomserverAPI struct {
	rsapi.ClientRoomserverAPI
	content map[string]any
}

func (s *joinContentRoomserverAPI) PerformJoin(_ context.Context, req *rsapi.PerformJoinRequest) (string, spec.ServerName, error) {
	s.content = req.Content
	return req.RoomIDOrAlias, "", nil
}

type globalProfileUserAPI struct {
	uapi.ClientUserAPI
}

func (globalProfileUserAPI) QueryProfile(context.Context, string) (*authtypes.Profile, error) {
	return &authtypes.Profile{DisplayName: "Global", AvatarURL: "mxc://test/global"}, nil
}

// A join sent as a state event sets the user's profile in the room, so the
// displayname and avatar_url it carries must survive; the global profile only
// fills in what it leaves out. The join endpoint keeps using the global one.
func TestJoinKeepsClientProfile(t *testing.T) {
	const roomID = "!room:remote.example"
	device := &uapi.Device{UserID: "@alice:test"}

	tests := []struct {
		name              string
		keepClientProfile bool
		body              string
		wantName          string
		wantAvatar        string
	}{
		{"state event keeps both", true, `{"membership":"join","displayname":"nick","avatar_url":"mxc://test/room"}`, "nick", "mxc://test/room"},
		{"state event fills in what is missing", true, `{"membership":"join","displayname":"nick"}`, "nick", "mxc://test/global"},
		{"join endpoint uses the global profile", false, `{"displayname":"nick"}`, "Global", "mxc://test/global"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := &joinContentRoomserverAPI{}
			req := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(tc.body))
			res := joinRoomByIDOrAlias(req, device, rs, globalProfileUserAPI{}, roomID, tc.keepClientProfile)
			if res.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200 (%+v)", res.Code, res.JSON)
			}
			if rs.content["displayname"] != tc.wantName || rs.content["avatar_url"] != tc.wantAvatar {
				t.Fatalf("content = %v, want displayname %q and avatar_url %q", rs.content, tc.wantName, tc.wantAvatar)
			}
		})
	}
}
