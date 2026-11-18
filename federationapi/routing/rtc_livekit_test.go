// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/federationapi/routing"
	"codefloe.com/pat-s/zendrite/internal/livekit"
	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
)

// liveKitRoomserver knows which servers and users are joined to rooms.
type liveKitRoomserver struct {
	roomserverAPI.FederationRoomserverAPI
	serverJoined bool
	userJoined   map[string]bool
	banned       bool
}

func (r *liveKitRoomserver) QueryServerBannedFromRoom(_ context.Context, _ *roomserverAPI.QueryServerBannedFromRoomRequest, res *roomserverAPI.QueryServerBannedFromRoomResponse) error {
	res.Banned = r.banned
	return nil
}

func (r *liveKitRoomserver) QueryServerJoinedToRoom(_ context.Context, _ *roomserverAPI.QueryServerJoinedToRoomRequest, res *roomserverAPI.QueryServerJoinedToRoomResponse) error {
	res.RoomExists, res.IsInRoom = r.serverJoined, r.serverJoined
	return nil
}

func (r *liveKitRoomserver) QueryMembershipForUser(_ context.Context, req *roomserverAPI.QueryMembershipForUserRequest, res *roomserverAPI.QueryMembershipForUserResponse) error {
	if r.userJoined[req.UserID.String()] {
		res.IsInRoom, res.Membership = true, spec.Join
	}
	return nil
}

// MSC4195: a server issues a token for its own SFU to a joined user of the
// requesting server.
func TestFederationLiveKitToken(t *testing.T) {
	sfu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer sfu.Close()
	cfg := &config.FederationAPI{Matrix: &config.Global{ServerName: "local"}}
	issuer := livekit.NewIssuer([]config.RTCFocus{{
		Type: "livekit", URL: "wss://sfu.local", LiveKitAPIKey: "key", LiveKitAPISecret: "secret", LiveKitAPIURL: sfu.URL,
	}})
	request := func(body string) *fclient.FederationRequest {
		req := fclient.NewFederationRequest(http.MethodPost, "remote", "local", "/_matrix/federation/v1/rtc/livekit/get_token")
		require.NoError(t, req.SetContent(gjson.Parse(body).Value()))
		return &req
	}
	const room = `"room_id":"!room:local","slot_id":"slot","member_id":"m"`
	rs := &liveKitRoomserver{serverJoined: true, userJoined: map[string]bool{"@alice:remote": true}}

	for _, tc := range []struct {
		name, body  string
		rs          *liveKitRoomserver
		wantCode    int
		wantErrCode string
	}{
		{"joined user", `{"user_id":"@alice:remote","url":"wss://sfu.local",` + room + `}`, rs, http.StatusOK, ""},
		{"user of another server", `{"user_id":"@bob:other","url":"wss://sfu.local",` + room + `}`, rs, http.StatusForbidden, "M_FORBIDDEN"},
		{"user not joined", `{"user_id":"@carol:remote","url":"wss://sfu.local",` + room + `}`, rs, http.StatusForbidden, "M_FORBIDDEN"},
		{"server not in the room", `{"user_id":"@alice:remote","url":"wss://sfu.local",` + room + `}`, &liveKitRoomserver{userJoined: rs.userJoined}, http.StatusForbidden, "M_FORBIDDEN"},
		{"server denied by the ACL", `{"user_id":"@alice:remote","url":"wss://sfu.local",` + room + `}`, &liveKitRoomserver{serverJoined: true, userJoined: rs.userJoined, banned: true}, http.StatusForbidden, "M_FORBIDDEN"},
		{"another SFU", `{"user_id":"@alice:remote","url":"wss://sfu.other",` + room + `}`, rs, http.StatusBadRequest, "M_INVALID_PARAM"},
		{"missing slot", `{"user_id":"@alice:remote","url":"wss://sfu.local","room_id":"!room:local","member_id":"m"}`, rs, http.StatusBadRequest, "M_MISSING_PARAM"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := routing.GetLiveKitToken(httptest.NewRequest(http.MethodPost, "/", nil), request(tc.body), cfg, tc.rs, issuer)
			require.Equal(t, tc.wantCode, res.Code)
			if tc.wantErrCode != "" {
				matrixErr, ok := res.JSON.(spec.MatrixError)
				require.True(t, ok)
				require.EqualValues(t, tc.wantErrCode, matrixErr.ErrCode)
				return
			}
			token, ok := res.JSON.(map[string]string)
			require.True(t, ok)
			require.NotEmpty(t, token["jwt"])
		})
	}
}
