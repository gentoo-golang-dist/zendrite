package streams

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"math"
	"strconv"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

type InviteStreamProvider struct {
	DefaultStreamProvider
	rsAPI api.SyncRoomserverAPI
}

func (p *InviteStreamProvider) Setup(
	ctx context.Context, snapshot storage.DatabaseTransaction,
) {
	p.DefaultStreamProvider.Setup(ctx, snapshot)

	p.latestMutex.Lock()
	defer p.latestMutex.Unlock()

	id, err := snapshot.MaxStreamPositionForInvites(ctx)
	if err != nil {
		panic(err)
	}
	p.latest = id
}

func (p *InviteStreamProvider) CompleteSync(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	req *types.SyncRequest,
) types.StreamPosition {
	return p.IncrementalSync(ctx, snapshot, req, 0, p.LatestPosition(ctx))
}

func (p *InviteStreamProvider) IncrementalSync(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	req *types.SyncRequest,
	from, to types.StreamPosition,
) types.StreamPosition {
	r := types.Range{
		From: from,
		To:   to,
	}

	invites, retiredInvites, maxID, err := snapshot.InviteEventsInRange(
		ctx, req.Device.UserID, r,
	)
	if err != nil {
		req.Log.WithError(err).Error("p.DB.InviteEventsInRange failed")
		return from
	}

	eventFormat := synctypes.FormatSync
	if req.Filter.EventFormat == synctypes.EventFormatFederation {
		eventFormat = synctypes.FormatSyncFederation
	}

	for roomID, inviteEvent := range invites {
		user := spec.UserID{}
		sender, err := p.rsAPI.QueryUserIDForSender(ctx, inviteEvent.RoomID(), inviteEvent.SenderID())
		if err == nil && sender != nil {
			user = *sender
		}

		// skip ignored user events
		if _, ok := req.IgnoredUsers.List[user.String()]; ok {
			continue
		}
		// Knocks share the invite stream; they belong under rooms.knock.
		if membership, _ := inviteEvent.Membership(); membership == spec.Knock {
			kr, err := types.NewKnockResponse(ctx, p.rsAPI, inviteEvent, eventFormat)
			if err != nil {
				req.Log.WithError(err).Error("failed creating knock response")
				continue
			}
			req.Response.Rooms.Knock[roomID] = kr
			continue
		}
		// Invites the user ignores or blocks are not served (MSC4380,
		// MSC4155).
		inviter := user.String()
		if err != nil || sender == nil {
			inviter = string(inviteEvent.SenderID())
		}
		if req.InviteConfig.Rule(inviter) != userapi.InviteAllow {
			continue
		}
		ir, err := types.NewInviteResponse(ctx, p.rsAPI, inviteEvent, eventFormat)
		if err != nil {
			req.Log.WithError(err).Error("failed creating invite response")
			continue
		}
		req.Response.Rooms.Invite[roomID] = ir
	}

	// A complete sync lists retired invites only for a filter with
	// include_leave, which asks for the rooms the user has left. Without it,
	// they would add rooms the user was invited to and has since left.
	completeSync := from == 0
	if completeSync && !req.Filter.Room.IncludeLeave {
		return to
	}
	for roomID := range retiredInvites {
		// The PDU stream already lists rooms whose leave the sync API has,
		// with the real leave event.
		if _, listed := req.Response.Rooms.Leave[roomID]; listed {
			continue
		}
		membership, _, err := snapshot.SelectMembershipForUser(ctx, roomID, req.Device.UserID, math.MaxInt64)
		// Skip if the user is an existing member of the room.
		// Otherwise, the NewLeaveResponse will eject the user from the room unintentionally
		if membership == spec.Join ||
			err != nil {
			continue
		}
		if completeSync && p.roomForgotten(ctx, req, roomID) {
			continue
		}

		lr := types.NewLeaveResponse()
		h := sha256.Sum256(append([]byte(roomID), []byte(strconv.FormatInt(int64(to), 10))...))
		lr.Timeline.Events = append(lr.Timeline.Events, synctypes.ClientEvent{
			// fake event ID which muxes in the to position
			EventID:        "$" + base64.RawURLEncoding.EncodeToString(h[:]),
			OriginServerTS: spec.AsTimestamp(time.Now()),
			RoomID:         roomID,
			Sender:         req.Device.UserID,
			StateKey:       &req.Device.UserID,
			Type:           "m.room.member",
			Content:        spec.RawJSON(`{"membership":"leave"}`),
		})
		req.Response.Rooms.Leave[roomID] = lr
	}

	if completeSync {
		return to
	}
	return maxID
}

// roomForgotten reports whether the user has forgotten the room, so that a
// complete sync leaves it out. A room whose membership cannot be looked up is
// listed, as the user's server may never have joined it.
func (p *InviteStreamProvider) roomForgotten(ctx context.Context, req *types.SyncRequest, roomID string) bool {
	userID, err := spec.NewUserID(req.Device.UserID, true)
	if err != nil {
		return false
	}
	var res api.QueryMembershipForUserResponse
	if err = p.rsAPI.QueryMembershipForUser(ctx, &api.QueryMembershipForUserRequest{
		RoomID: roomID, UserID: *userID,
	}, &res); err != nil {
		req.Log.WithError(err).WithField("room_id", roomID).Debug("Failed to query the membership of a room with a retired invite")
		return false
	}
	return res.IsRoomForgotten
}
