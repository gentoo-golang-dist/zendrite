// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sync

import (
	"context"
	"encoding/json"
	"slices"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

// syncProfileUpdates adds the MSC4429 profile updates to a /sync response
// whose filter asks for profile fields, with the response shape of Synapse,
// and returns the profile position the response served up to. It runs after
// the other streams, whose rooms it reads; toPos holds the positions they
// served.
//
// Only the fields the filter lists are sent:
//
//   - On an initial sync, the syncing user's own profile.
//   - The whole profiles of the joined members whose membership event a
//     joined room of the response carries in its state, or on an initial
//     sync in its timeline: every member of a room without lazy-loaded
//     members, and with them the members the response loads.
//   - The changed fields of every local user who shares a joined room with
//     the syncing user, the syncing user included, with null for removed
//     fields.
//   - Whole profiles of users who join a shared room, the display name and
//     avatar URL changes of remote users, and null for users the syncing
//     user no longer shares any room with, as in the MSC4262 sliding sync
//     extension.
func (rp *RequestPool) syncProfileUpdates(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	syncReq *types.SyncRequest,
	initial bool,
	toPos types.StreamingToken,
) types.StreamPosition {
	ids := syncReq.Filter.ProfileFieldIDs()
	wanted := func(field string) bool { return slices.Contains(ids, field) }
	userID := syncReq.Device.UserID
	served := toPos.ProfilePosition
	failed := func(err error, msg string) types.StreamPosition {
		syncReq.Log.WithError(err).Error(msg)
		return syncReq.Since.ProfilePosition
	}

	targets := newProfileTargets()
	rp.addResponseMembers(syncReq.Response, initial, targets)
	if initial {
		targets.addFull(userID, "")
	} else {
		joinedRooms, err := snapshot.RoomIDsWithMembership(ctx, userID, spec.Join)
		if err != nil {
			return failed(err, "[PROFILES] Failed to load joined rooms")
		}
		if served, err = rp.collectProfileChanges(ctx, snapshot, userID, &syncReq.Since, toPos, joinedRooms, targets); err != nil {
			return failed(err, "[PROFILES] Failed to load profile changes")
		}
	}

	users, err := rp.resolveProfileTargets(ctx, snapshot, targets, wanted)
	if err != nil {
		return failed(err, "[PROFILES] Failed to load profiles")
	}
	if len(users) == 0 {
		return served
	}
	syncReq.Response.Users = make(map[string]*types.SyncUserUpdate, len(users))
	for user, update := range users {
		if update == nil {
			syncReq.Response.Users[user] = &types.SyncUserUpdate{}
			continue
		}
		fields := make(map[string]json.RawMessage, len(update.Updated)+len(update.Removed))
		for field, value := range update.Updated {
			fields[field] = value
		}
		// /sync has no removed list: a removed field is sent as null.
		for _, field := range update.Removed {
			fields[field] = json.RawMessage("null")
		}
		syncReq.Response.Users[user] = &types.SyncUserUpdate{ProfileUpdates: fields}
	}
	return served
}

// addResponseMembers adds to targets the whole profiles of the joined members
// whose membership event a joined room of the response carries in its state,
// or with timeline set, also in its timeline. The display name and avatar
// URL of remote members are taken from the latest of their membership events
// in the response, so that they are not loaded again.
func (rp *RequestPool) addResponseMembers(resp *types.Response, timeline bool, targets *profileTargets) {
	if resp == nil || resp.Rooms == nil {
		return
	}
	for roomID, room := range resp.Rooms.Join {
		if room == nil {
			continue
		}
		var state, events []synctypes.ClientEvent
		if room.State != nil {
			state = room.State.Events
		}
		if room.Timeline != nil {
			events = room.Timeline.Events
		}
		for _, ev := range state {
			rp.addResponseMember(ev, roomID, true, targets)
		}
		for _, ev := range events {
			rp.addResponseMember(ev, roomID, timeline, targets)
		}
	}
}

// addResponseMember records the profile of a remote user's membership event
// and, with add set, adds its joined member to targets.
func (rp *RequestPool) addResponseMember(ev synctypes.ClientEvent, roomID string, add bool, targets *profileTargets) {
	if ev.Type != spec.MRoomMember || ev.StateKey == nil {
		return
	}
	member := *ev.StateKey
	if gjson.GetBytes(ev.Content, "membership").Str != spec.Join {
		return
	}
	if !rp.isLocalUser(member) {
		profile := make(map[string]json.RawMessage, 2)
		for _, field := range []string{authtypes.ProfileFieldDisplayName, authtypes.ProfileFieldAvatarURL} {
			value := gjson.GetBytes(ev.Content, field)
			if value.Type == gjson.String && value.Str != "" {
				if encoded, err := json.Marshal(value.Str); err == nil {
					profile[field] = encoded
				}
			}
		}
		targets.known[member] = profile
	}
	if add {
		targets.addFull(member, roomID)
	}
}
