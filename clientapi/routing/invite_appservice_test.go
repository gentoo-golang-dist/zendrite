// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only.

package routing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	appserviceAPI "codefloe.com/pat-s/zendrite/appservice/api"
	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// inviteAppServiceAPI records which users the application services are
// asked about.
type inviteAppServiceAPI struct {
	appserviceAPI.AppServiceInternalAPI
	asked []string
}

func (a *inviteAppServiceAPI) UserIDExists(_ context.Context, req *appserviceAPI.UserIDExistsRequest, res *appserviceAPI.UserIDExistsResponse) error {
	a.asked = append(a.asked, req.UserID)
	res.UserIDExists = false
	return nil
}

// inviteProfileAPI knows the profiles of the users in known.
type inviteProfileAPI struct {
	api.ClientUserAPI
	known map[string]bool
}

func (p *inviteProfileAPI) QueryProfile(_ context.Context, userID string) (*authtypes.Profile, error) {
	if p.known[userID] {
		return &authtypes.Profile{}, nil
	}
	return nil, appserviceAPI.ErrProfileNotExists
}

// inviteRoomserverAPI lets the inviter invite and records the invitees.
type inviteRoomserverAPI struct {
	roomserverAPI.ClientRoomserverAPI
	invited []string
}

func (inviteRoomserverAPI) QueryMembershipForUser(_ context.Context, _ *roomserverAPI.QueryMembershipForUserRequest, res *roomserverAPI.QueryMembershipForUserResponse) error {
	res.IsInRoom = true
	return nil
}

func (r *inviteRoomserverAPI) PerformInvite(_ context.Context, req *roomserverAPI.PerformInviteRequest) error {
	r.invited = append(r.invited, req.InviteInput.Invitee.String())
	return nil
}

// TestInviteAsksAppServiceForUnknownLocalUser checks that inviting a local
// user the server does not know asks the application services about the
// user first, as the application service API says a room invite should, so
// an application service can register its user before the invite arrives.
func TestInviteAsksAppServiceForUnknownLocalUser(t *testing.T) {
	for _, tc := range []struct {
		name    string
		invitee string
		asked   []string
	}{
		{name: "unknown local user", invitee: "@ghost:test", asked: []string{"@ghost:test"}},
		{name: "known local user", invitee: "@bob:test"},
		{name: "remote user", invitee: "@carol:remote.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asAPI := &inviteAppServiceAPI{}
			profileAPI := &inviteProfileAPI{known: map[string]bool{"@bob:test": true}}
			rsAPI := &inviteRoomserverAPI{}
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"user_id":"`+tc.invitee+`"}`))

			res := SendInvite(req, profileAPI, &api.Device{UserID: "@alice:test"}, "!room:test", threePIDConfig(), rsAPI, asAPI, nil)

			require.Equal(t, http.StatusOK, res.Code, "%+v", res.JSON)
			require.Equal(t, tc.asked, asAPI.asked)
			require.Equal(t, []string{tc.invitee}, rsAPI.invited)
		})
	}
}
