// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"
	"fmt"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/roomserver/api"
)

// QueryLatestEventsAndState implements api.QueryLatestEventsAndStateAPI.
//
// The partial state of a room joined with a faster join leaves out the
// memberships of remote users (MSC3902), so an event built on it whose auth
// needs one of them, such as the invite, kick or ban of a remote user, could
// be authorized wrongly: a ban, say, would not stop an invite. Such an event
// waits for the full state when the partial state lacks the membership.
func (r *RoomserverInternalAPI) QueryLatestEventsAndState(
	ctx context.Context,
	req *api.QueryLatestEventsAndStateRequest,
	res *api.QueryLatestEventsAndStateResponse,
) error {
	if err := r.Queryer.QueryLatestEventsAndState(ctx, req, res); err != nil || !res.RoomExists {
		return err
	}
	if !r.lacksRemoteMembership(req.StateToFetch, res) {
		return nil
	}
	partial, err := r.IsRoomIDPartialState(ctx, req.RoomID)
	if err != nil || !partial {
		return err
	}
	if err = r.AwaitFullState(ctx, req.RoomID); err != nil {
		return fmt.Errorf("r.AwaitFullState: %w", err)
	}
	*res = api.QueryLatestEventsAndStateResponse{}
	return r.Queryer.QueryLatestEventsAndState(ctx, req, res)
}

// lacksRemoteMembership reports whether the state asked for holds the
// membership of a remote user that the response does not have.
func (r *RoomserverInternalAPI) lacksRemoteMembership(
	tuples []gomatrixserverlib.StateKeyTuple, res *api.QueryLatestEventsAndStateResponse,
) bool {
	for _, tuple := range tuples {
		if tuple.EventType != spec.MRoomMember {
			continue
		}
		userID, err := spec.NewUserID(tuple.StateKey, true)
		if err != nil || r.Cfg.Global.IsLocalServerName(userID.Domain()) {
			continue
		}
		found := false
		for _, ev := range res.StateEvents {
			if ev.Type() == spec.MRoomMember && ev.StateKeyEquals(tuple.StateKey) {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}
