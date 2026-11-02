// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	appserviceAPI "codefloe.com/pat-s/zendrite/appservice/api"
	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

// profilesStream is the SlidingSyncConnectionStream "stream" tag recording,
// per room, that the room's member profiles were sent on this connection.
// Its last token is the key of the fields they were sent with, so that a
// change of fields sends them again.
const profilesStream = "profiles"

// profileUpdatesQueryLimit caps the profile changes one response reads. A
// response that hits it serves the stream only up to the last change it read,
// and the rest follow in the next response.
const profileUpdatesQueryLimit = 1000

// profileMembershipQueryLimit caps the membership events per room that one
// response inspects for users who start or stop sharing a room.
const profileMembershipQueryLimit = 1000

// profileFieldsKey identifies a fields filter, nil meaning every field.
func profileFieldsKey(fields *[]string) string {
	if fields == nil {
		return "*"
	}
	sorted := slices.Clone(*fields)
	sort.Strings(sorted)
	sorted = slices.Compact(sorted)
	key, err := json.Marshal(sorted)
	if err != nil {
		// Strings always encode; send the profiles again rather than never.
		return ""
	}
	return string(key)
}

// lazyMembers reports whether required state loads room members lazily.
func lazyMembers(requiredState types.RequiredStateConfig) bool {
	if requiredState.LazyMembers {
		return true
	}
	for _, pattern := range requiredState.Include {
		if len(pattern) == 2 && pattern[0] == spec.MRoomMember && pattern[1] == "$LAZY" {
			return true
		}
	}
	return false
}

// profileTargets collects whose profiles a response carries.
type profileTargets struct {
	// full holds the users whose whole profile is sent.
	full map[string]bool
	// changed holds, per user, the fields that changed since the last
	// response, which are sent alone.
	changed map[string]map[string]bool
	// gone holds the users the syncing user no longer shares a room with.
	gone map[string]bool
	// roomOf holds a room shared with each remote user, whose membership
	// event provides the profile.
	roomOf map[string]string
}

func (t *profileTargets) addFull(userID, roomID string) {
	t.full[userID] = true
	if roomID != "" {
		t.roomOf[userID] = roomID
	}
}

func (t *profileTargets) addChanged(userID, field, roomID string) {
	if t.changed[userID] == nil {
		t.changed[userID] = make(map[string]bool)
	}
	t.changed[userID][field] = true
	if roomID != "" {
		t.roomOf[userID] = roomID
	}
}

// processProfilesExtension handles the MSC4262 profile updates extension,
// with the request and response shape of Synapse. It returns the profile
// position the response served up to.
//
//   - Whole profiles of the members of a joined room in the response the
//     first time the room is sent on this connection, or when the fields
//     filter changed since it was. For rooms whose members load lazily, only
//     the users the room's response mentions.
//   - The syncing user's own profile on an initial sync.
//   - The fields that changed of every local user who shares a joined room
//     with the syncing user, the syncing user included, whether or not the
//     room is in the response.
//   - Whole profiles of users who join a room shared with the syncing user.
//   - The display name and avatar URL of remote users whose membership event
//     changed them.
//   - null for users the syncing user no longer shares any room with.
func (rp *RequestPool) processProfilesExtension(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	connState *V4ConnectionState,
	userID string,
	req *types.ProfilesRequest,
	v4Req *types.SlidingSyncRequest,
	fromPos *types.StreamingToken,
	toPos types.StreamingToken,
	responseLists map[string]types.SlidingList,
	roomSubscriptions map[string]bool,
	responseRooms map[string]types.SlidingRoomData,
) (*types.ProfilesResponse, types.StreamPosition, error) {
	served := toPos.ProfilePosition
	fieldsKey := profileFieldsKey(req.Fields)
	wanted := func(field string) bool {
		return req.Fields == nil || slices.Contains(*req.Fields, field)
	}
	targets := &profileTargets{
		full:    make(map[string]bool),
		changed: make(map[string]map[string]bool),
		gone:    make(map[string]bool),
		roomOf:  make(map[string]string),
	}

	joinedRooms, err := snapshot.RoomIDsWithMembership(ctx, userID, spec.Join)
	if err != nil {
		return nil, served, fmt.Errorf("load joined rooms: %w", err)
	}
	joined := make(map[string]bool, len(joinedRooms))
	for _, roomID := range joinedRooms {
		joined[roomID] = true
	}

	// The joined rooms of the response that the extension covers.
	var relevant map[string]bool
	if req.Lists != nil || req.Rooms != nil {
		relevant = findRelevantRoomIDsForExtension(req.Lists, req.Rooms, responseLists, roomSubscriptions)
	}
	subset := make([]string, 0, len(responseRooms))
	for roomID := range responseRooms {
		if joined[roomID] && (relevant == nil || relevant[roomID]) {
			subset = append(subset, roomID)
		}
	}
	sort.Strings(subset)

	// As in Synapse, a room loads its members lazily when any list or room
	// subscription covering it does.
	lazyRooms := make(map[string]bool)
	for name, list := range v4Req.Lists {
		if lazyMembers(list.RequiredState) {
			for _, roomID := range responseLists[name].MatchedRoomIDs {
				lazyRooms[roomID] = true
			}
		}
	}
	for roomID, sub := range v4Req.RoomSubscriptions {
		if lazyMembers(sub.RequiredState) {
			lazyRooms[roomID] = true
		}
	}

	var newRooms []string
	for _, roomID := range subset {
		sentBefore := profilesSentBefore(connState, roomID, fieldsKey)
		if lazyRooms[roomID] {
			for _, member := range lazyProfileUsers(responseRooms[roomID], !sentBefore) {
				targets.addFull(member, roomID)
			}
			continue
		}
		if !sentBefore {
			newRooms = append(newRooms, roomID)
		}
	}
	if len(newRooms) > 0 {
		members, membersErr := snapshot.AllJoinedUsersInRoom(ctx, newRooms)
		if membersErr != nil {
			return nil, served, fmt.Errorf("load room members: %w", membersErr)
		}
		for roomID, userIDs := range members {
			for _, member := range userIDs {
				targets.addFull(member, roomID)
			}
		}
	}
	if fromPos == nil {
		targets.addFull(userID, "")
	} else {
		if served, err = rp.collectProfileChanges(ctx, snapshot, userID, fromPos, toPos, joinedRooms, targets); err != nil {
			return nil, toPos.ProfilePosition, err
		}
	}

	resp := &types.ProfilesResponse{Users: make(map[string]*types.ProfileUserUpdate)}
	for gone := range targets.gone {
		resp.Users[gone] = nil
	}
	users := make(map[string]bool, len(targets.full)+len(targets.changed))
	for user := range targets.full {
		users[user] = true
	}
	for user := range targets.changed {
		users[user] = true
	}
	for user := range users {
		if targets.gone[user] {
			continue
		}
		if err = ctx.Err(); err != nil {
			return nil, served, err
		}
		profile, ok := rp.profileFields(ctx, snapshot, user, targets.roomOf[user])
		if !ok {
			continue
		}
		update := &types.ProfileUserUpdate{}
		fields := targets.changed[user]
		if targets.full[user] {
			fields = make(map[string]bool, len(profile)+len(targets.changed[user]))
			for field := range profile {
				fields[field] = true
			}
			for field := range targets.changed[user] {
				fields[field] = true
			}
		}
		for field := range fields {
			if !wanted(field) {
				continue
			}
			if value, set := profile[field]; set {
				if update.Updated == nil {
					update.Updated = make(map[string]json.RawMessage)
				}
				update.Updated[field] = value
			} else if targets.changed[user][field] {
				update.Removed = append(update.Removed, field)
			}
		}
		if len(update.Updated) == 0 && len(update.Removed) == 0 {
			continue
		}
		sort.Strings(update.Removed)
		resp.Users[user] = update
	}

	rp.persistProfilesRoomState(ctx, connState, subset, fieldsKey)
	if len(resp.Users) == 0 {
		return nil, served, nil
	}
	return resp, served, nil
}

// profilesSentBefore reports whether this connection already sent the member
// profiles of the room with the given fields.
func profilesSentBefore(connState *V4ConnectionState, roomID, fieldsKey string) bool {
	if connState == nil || connState.PreviousStreamStates == nil {
		return false
	}
	prevState, ok := connState.PreviousStreamStates[roomID][profilesStream]
	return ok && prevState.LastToken == fieldsKey
}

// lazyProfileUsers returns the users a room's response mentions when its
// members load lazily: timeline senders and member state, and with
// withHeroes, which is set the first time the room's profiles are sent on
// the connection, its heroes. Synapse sends the heroes every time, but every
// response for the room carries them, so that would resend their profiles
// in every response.
func lazyProfileUsers(room types.SlidingRoomData, withHeroes bool) []string {
	var users []string
	for _, ev := range room.Timeline {
		users = append(users, ev.Sender)
	}
	for _, ev := range room.RequiredState {
		if ev.Type == spec.MRoomMember && ev.StateKey != nil {
			users = append(users, *ev.StateKey)
		}
	}
	if withHeroes {
		for _, hero := range room.Heroes {
			users = append(users, hero.UserID)
		}
	}
	return users
}

// collectProfileChanges adds the profile changes since fromPos to targets:
// local users' changed fields from the profile updates stream, and from the
// membership events of the syncing user's rooms, users who joined, remote
// users who changed their name or avatar, and users who no longer share a
// room. It returns the profile position it read up to.
func (rp *RequestPool) collectProfileChanges(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	userID string,
	fromPos *types.StreamingToken,
	toPos types.StreamingToken,
	joinedRooms []string,
	targets *profileTargets,
) (types.StreamPosition, error) {
	served := toPos.ProfilePosition
	updates, err := snapshot.ProfileUpdatesInRange(ctx, fromPos.ProfilePosition, toPos.ProfilePosition, profileUpdatesQueryLimit)
	if err != nil {
		return served, fmt.Errorf("load profile updates: %w", err)
	}
	if len(updates) == profileUpdatesQueryLimit {
		served = updates[len(updates)-1].Position
	}
	var updatedUsers []string
	for _, update := range updates {
		if update.UserID != userID {
			updatedUsers = append(updatedUsers, update.UserID)
		}
	}
	shared, err := sharedUserSet(ctx, snapshot, userID, updatedUsers)
	if err != nil {
		return served, err
	}
	for _, update := range updates {
		if update.UserID == userID || shared[update.UserID] {
			targets.addChanged(update.UserID, update.Field, "")
		}
	}

	if toPos.PDUPosition <= fromPos.PDUPosition {
		return served, nil
	}
	leftRooms, err := snapshot.RoomIDsWithMembership(ctx, userID, spec.Leave)
	if err != nil {
		return served, fmt.Errorf("load left rooms: %w", err)
	}
	bannedRooms, err := snapshot.RoomIDsWithMembership(ctx, userID, spec.Ban)
	if err != nil {
		return served, fmt.Errorf("load banned rooms: %w", err)
	}
	rooms := slices.Concat(joinedRooms, leftRooms, bannedRooms)
	if rooms, err = snapshot.RoomsWithEventsSince(ctx, rooms, fromPos.PDUPosition); err != nil {
		return served, fmt.Errorf("load changed rooms: %w", err)
	}
	if len(rooms) == 0 {
		return served, nil
	}
	memberTypes := []string{spec.MRoomMember}
	recent, err := snapshot.RecentEvents(ctx, rooms, types.Range{From: fromPos.PDUPosition, To: toPos.PDUPosition}, &synctypes.RoomEventFilter{
		Limit: profileMembershipQueryLimit,
		Types: &memberTypes,
	}, true, true)
	if err != nil {
		return served, fmt.Errorf("load membership events: %w", err)
	}

	joinedNow := make(map[string]bool, len(joinedRooms))
	for _, roomID := range joinedRooms {
		joinedNow[roomID] = true
	}
	joinedUsers := make(map[string]string)
	leftUsers := make(map[string]bool)
	var roomsLeft []string
	// Users who joined a room after the syncing user left it, whom the
	// syncing user never shared it with.
	joinedAfterLeave := make(map[string]map[string]bool)
	for roomID, events := range recent {
		left := false
		for _, ev := range events.Events {
			stateKey := ev.StateKey()
			if stateKey == nil || ev.Type() != spec.MRoomMember {
				continue
			}
			member := *stateKey
			membership := gjson.GetBytes(ev.Content(), "membership").Str
			prevContent := gjson.GetBytes(ev.Unsigned(), "prev_content")
			prevMembership := prevContent.Get("membership").Str
			if member == userID {
				if (membership == spec.Leave || membership == spec.Ban) && prevMembership == spec.Join {
					roomsLeft = append(roomsLeft, roomID)
					left = true
				}
				continue
			}
			// Only the syncing user's own leave counts in a room they are no
			// longer in: membership changes after it are not theirs to see.
			if !joinedNow[roomID] {
				if left && membership == spec.Join {
					if joinedAfterLeave[roomID] == nil {
						joinedAfterLeave[roomID] = make(map[string]bool)
					}
					joinedAfterLeave[roomID][member] = true
				}
				continue
			}
			switch membership {
			case spec.Join:
				if prevMembership != spec.Join {
					joinedUsers[member] = roomID
					continue
				}
				if rp.isLocalUser(member) {
					// The profile updates stream carries local users' changes.
					continue
				}
				for _, field := range []string{authtypes.ProfileFieldDisplayName, authtypes.ProfileFieldAvatarURL} {
					if gjson.GetBytes(ev.Content(), field).Str != prevContent.Get(field).Str {
						targets.addChanged(member, field, roomID)
					}
				}
			case spec.Leave, spec.Ban:
				leftUsers[member] = true
			}
		}
	}
	if len(roomsLeft) > 0 {
		members, membersErr := snapshot.AllJoinedUsersInRoom(ctx, roomsLeft)
		if membersErr != nil {
			return served, fmt.Errorf("load members of left rooms: %w", membersErr)
		}
		for roomID, userIDs := range members {
			for _, member := range userIDs {
				if !joinedAfterLeave[roomID][member] {
					leftUsers[member] = true
				}
			}
		}
	}
	candidates := make([]string, 0, len(joinedUsers)+len(leftUsers))
	for member := range joinedUsers {
		candidates = append(candidates, member)
	}
	for member := range leftUsers {
		candidates = append(candidates, member)
	}
	for member := range targets.changed {
		if !rp.isLocalUser(member) {
			candidates = append(candidates, member)
		}
	}
	if shared, err = sharedUserSet(ctx, snapshot, userID, candidates); err != nil {
		return served, err
	}
	for member, roomID := range joinedUsers {
		if roomID != "" && shared[member] {
			targets.addFull(member, roomID)
		}
	}
	for _, member := range candidates {
		if member != userID && !shared[member] {
			targets.gone[member] = true
		}
	}
	return served, nil
}

// sharedUserSet returns which of the users share a room with userID.
func sharedUserSet(ctx context.Context, snapshot storage.DatabaseTransaction, userID string, users []string) (map[string]bool, error) {
	set := make(map[string]bool, len(users))
	if len(users) == 0 {
		return set, nil
	}
	shared, err := snapshot.SharedUsers(ctx, userID, users)
	if err != nil {
		return nil, fmt.Errorf("load shared users: %w", err)
	}
	for _, user := range shared {
		set[user] = true
	}
	return set, nil
}

func (rp *RequestPool) isLocalUser(userID string) bool {
	parsed, err := spec.NewUserID(userID, true)
	return err == nil && rp.cfg.Matrix.IsLocalServerName(parsed.Domain())
}

// profileFields returns a user's profile fields as JSON values, leaving out
// an empty display name or avatar URL. A local user's profile comes from the
// user API. For a remote user, whose global profile the server does not
// track, it is the display name and avatar URL of their membership event in
// roomID. It reports false when the profile could not be read.
func (rp *RequestPool) profileFields(
	ctx context.Context, snapshot storage.DatabaseTransaction, userID, roomID string,
) (map[string]json.RawMessage, bool) {
	fields := make(map[string]json.RawMessage)
	setString := func(field, value string) {
		if value == "" {
			return
		}
		if encoded, err := json.Marshal(value); err == nil {
			fields[field] = encoded
		}
	}
	if rp.isLocalUser(userID) {
		profile, err := rp.userAPI.QueryProfile(ctx, userID)
		switch {
		case errors.Is(err, appserviceAPI.ErrProfileNotExists):
			return fields, true
		case err != nil:
			logrus.WithError(err).WithField("user_id", userID).Warn("[PROFILES] Failed to load profile")
			return nil, false
		}
		for field, value := range profile.Fields {
			fields[field] = value
		}
		setString(authtypes.ProfileFieldDisplayName, profile.DisplayName)
		setString(authtypes.ProfileFieldAvatarURL, profile.AvatarURL)
		return fields, true
	}
	if roomID == "" {
		return nil, false
	}
	ev, err := snapshot.GetStateEvent(ctx, roomID, spec.MRoomMember, userID)
	if err != nil {
		logrus.WithError(err).WithField("user_id", userID).Warn("[PROFILES] Failed to load membership event")
		return nil, false
	}
	if ev == nil {
		return fields, true
	}
	setString(authtypes.ProfileFieldDisplayName, gjson.GetBytes(ev.Content(), authtypes.ProfileFieldDisplayName).Str)
	setString(authtypes.ProfileFieldAvatarURL, gjson.GetBytes(ev.Content(), authtypes.ProfileFieldAvatarURL).Str)
	return fields, true
}

// persistProfilesRoomState records that the member profiles of the rooms were
// sent with the given fields, at this response's connection position. The
// client acknowledges them by syncing from it; a retry from the previous
// position finds them unsent and sends them again.
//
// Rooms it does not write are carried over by copyForwardProfilesState, which
// runs for every response.
func (rp *RequestPool) persistProfilesRoomState(ctx context.Context, connState *V4ConnectionState, roomIDs []string, fieldsKey string) {
	if connState == nil || connState.ConnectionPosition == 0 {
		return
	}
	if connState.ProfilesWritten == nil {
		connState.ProfilesWritten = make(map[string]bool, len(roomIDs))
	}
	for _, roomID := range roomIDs {
		if ctx.Err() != nil {
			return
		}
		if err := rp.db.UpdateConnectionStream(ctx, connState.ConnectionPosition, roomID, profilesStream, "live", fieldsKey); err != nil {
			if ctx.Err() != nil {
				return
			}
			logrus.WithError(err).WithField("room_id", roomID).Warn("[PROFILES] Failed to persist profiles stream state")
			continue
		}
		connState.ProfilesWritten[roomID] = true
	}
}

// copyForwardProfilesState carries every room's profiles stream state onto
// this connection position, for rooms persistProfilesRoomState did not
// already write, like copyForwardAccountDataState.
func (rp *RequestPool) copyForwardProfilesState(ctx context.Context, connState *V4ConnectionState) error {
	if connState == nil || connState.ConnectionPosition == 0 {
		return nil
	}
	for roomID, streams := range connState.PreviousStreamStates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if connState.ProfilesWritten[roomID] {
			continue
		}
		prevState, ok := streams[profilesStream]
		if !ok {
			continue
		}
		if err := rp.db.UpdateConnectionStream(ctx, connState.ConnectionPosition, roomID, profilesStream, prevState.RoomStatus, prevState.LastToken); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			logrus.WithError(err).WithField("room_id", roomID).Warn("[PROFILES] Failed to copy forward profiles stream state")
		}
	}
	return nil
}
