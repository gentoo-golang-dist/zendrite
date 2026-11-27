// Copyright 2024 New Vector Ltd.
// Copyright 2023 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/tidwall/gjson"

	fs "codefloe.com/pat-s/zendrite/federationapi/api"
	roomserver "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// Traverse the room hierarchy using the provided walker up to the provided limit,
// returning a new walker which can be used to fetch the next page.
//
// If limit is -1, this is treated as no limit, and the entire hierarchy will be traversed.
//
// If returned walker is nil, then there are no more rooms left to traverse. This method does not modify the provided walker, so it
// can be cached.
//
//nolint:gocyclo
func (querier *Queryer) QueryNextRoomHierarchyPage(ctx context.Context, walker roomserver.RoomHierarchyWalker, limit int) (
	[]fclient.RoomHierarchyRoom,
	[]string,
	*roomserver.RoomHierarchyWalker,
	error,
) {
	// A root that the client's vias provided was checked on the first page.
	var remoteRoot *fclient.RoomHierarchyResponse
	if !walker.RootRemote {
		if authorised, _ := authorised(ctx, querier, walker.Caller, walker.RootRoomID); !authorised { //nolint:misspell
			remoteRoot = remoteRootAccessible(ctx, querier, walker)
			if remoteRoot == nil {
				return nil, []string{walker.RootRoomID.String()}, nil, roomserver.ErrRoomUnknownOrNotAllowed{Err: fmt.Errorf("room is unknown/forbidden")}
			}
			walker.RootRemote = true
		}
	}

	discoveredRooms := []fclient.RoomHierarchyRoom{}

	// Copy unvisited and processed to avoid modifying original walker (which is typically in cache)
	unvisited := make([]roomserver.RoomHierarchyWalkerQueuedRoom, 0, len(walker.Unvisited))
	unvisited = append(unvisited, walker.Unvisited...)
	processed := walker.Processed.Copy()
	inaccessible := []string{}

	// Depth first -> stack data structure
	for len(unvisited) > 0 {
		if len(discoveredRooms) >= limit && limit != -1 {
			break
		}

		// If the context is canceled, we might still have discovered rooms
		// return them to the client and let the client know there _may_ be more rooms.
		if errors.Is(ctx.Err(), context.Canceled) {
			break
		}

		// pop the stack
		queuedRoom := unvisited[len(unvisited)-1]
		unvisited = unvisited[:len(unvisited)-1]
		// If this room has already been processed, skip.
		// If this room exceeds the specified depth, skip.
		if processed.Contains(queuedRoom.RoomID) || (walker.MaxDepth > 0 && queuedRoom.Depth > walker.MaxDepth) {
			continue
		}

		// Mark this room as processed.
		processed.Add(queuedRoom.RoomID)

		// if this room is not a space room, skip.
		var roomType *string
		create := stateEvent(ctx, querier, queuedRoom.RoomID, spec.MRoomCreate, "")
		if create != nil {
			var createContent gomatrixserverlib.CreateContent
			err := json.Unmarshal(create.Content(), &createContent)
			if err != nil {
				util.GetLogger(ctx).WithError(err).WithField("create_content", create.Content()).Warn("failed to unmarshal m.room.create event")
			}
			if createContent.RoomType != "" {
				roomType = &createContent.RoomType
			}
		}

		// Collect rooms/events to send back (either locally or fetched via federation)
		var discoveredChildEvents []fclient.RoomHierarchyStrippedEvent

		// If we know about this room and the caller may see it (see userMaySeeRoom) then pull
		// events locally
		roomExists := roomExists(ctx, querier, queuedRoom.RoomID)
		if !roomExists {
			// attempt to query this room over federation, as either we've never heard of it before
			// or we've left it and hence are not authorized (but info may be exposed regardless)
			fedRes := remoteRoot
			if fedRes == nil || queuedRoom.RoomID != walker.RootRoomID {
				// Answers from servers that the client chose must not reach the
				// caches that other walks read.
				fedRes = federatedRoomInfo(ctx, querier, walker.Caller, walker.SuggestedOnly, queuedRoom.RoomID, queuedRoom.ParentRoomID, queuedRoom.Vias, !walker.RootRemote)
			}
			if fedRes != nil {
				discoveredChildEvents = fedRes.Room.ChildrenState
				discoveredRooms = append(discoveredRooms, fedRes.Room)
				// mark this room as a space room as the federated server responded.
				// we need to do this so we add the children of this room to the unvisited stack
				// as these children may be rooms we do know about.
				spaceType := spec.MSpace
				roomType = &spaceType
			}
		} else if authorised, allowedRoomIDs := authorised(ctx, querier, walker.Caller, queuedRoom.RoomID); authorised { //nolint:misspell
			// Get all `m.space.child` state events for this room
			events, err := childReferences(ctx, querier, walker.SuggestedOnly, queuedRoom.RoomID)
			if err != nil {
				util.GetLogger(ctx).WithError(err).WithField("room_id", queuedRoom.RoomID).Error("failed to extract references for room")
				continue
			}
			discoveredChildEvents = events

			pubRoom := publicRoomsChunk(ctx, querier, queuedRoom.RoomID)

			if pubRoom == nil {
				util.GetLogger(ctx).WithField("room_id", queuedRoom.RoomID).Debug("unable to get publicRoomsChunk")
				continue
			}

			hierarchyRoom := fclient.RoomHierarchyRoom{
				PublicRoom:     *pubRoom,
				RoomType:       roomType,
				ChildrenState:  events,
				AllowedRoomIDs: allowedRoomIDs,
			}
			// MSC3266 adds the room version and encryption algorithm.
			if create != nil {
				hierarchyRoom.RoomVersion = string(create.Version())
			}
			if encryption := stateEvent(ctx, querier, queuedRoom.RoomID, spec.MRoomEncryption, ""); encryption != nil {
				hierarchyRoom.Encryption = gjson.GetBytes(encryption.Content(), "algorithm").Str
			}
			discoveredRooms = append(discoveredRooms, hierarchyRoom)
		} else {
			inaccessible = append(inaccessible, queuedRoom.RoomID.String())
			continue
		}

		// don't walk the children
		// if the parent is not a space room
		if roomType == nil || *roomType != spec.MSpace {
			continue
		}

		// For each referenced room ID in the child events being returned to the caller
		// add the room ID to the queue of unvisited rooms. Loop from the beginning.
		// We need to invert the order here because the child events are lo->hi on the timestamp,
		// so we need to ensure we pop in the same lo->hi order, which won't be the case if we
		// insert the highest timestamp last in a stack.
	extendQueueLoop:
		for i := len(discoveredChildEvents) - 1; i >= 0; i-- {
			spaceContent := struct {
				Via []string `json:"via"`
			}{}
			ev := discoveredChildEvents[i]
			_ = json.Unmarshal(ev.Content, &spaceContent)

			childRoomID, err := spec.NewRoomID(ev.StateKey)

			if err != nil {
				util.GetLogger(ctx).WithError(err).WithField("invalid_room_id", ev.StateKey).WithField("parent_room_id", queuedRoom.RoomID).Warn("Invalid room ID in m.space.child state event")
			} else {
				// Make sure not to queue inaccessible rooms
				for _, inaccessibleRoomID := range inaccessible {
					if inaccessibleRoomID == childRoomID.String() {
						continue extendQueueLoop
					}
				}
				unvisited = append(unvisited, roomserver.RoomHierarchyWalkerQueuedRoom{
					RoomID:       *childRoomID,
					ParentRoomID: &queuedRoom.RoomID,
					Depth:        queuedRoom.Depth + 1,
					Vias:         spaceContent.Via,
				})
			}
		}
	}

	// Deduplicate rooms - federated responses may include rooms we've already discovered
	// via other paths in the hierarchy
	seenRooms := make(map[string]bool, len(discoveredRooms))
	deduplicatedRooms := make([]fclient.RoomHierarchyRoom, 0, len(discoveredRooms))
	for _, room := range discoveredRooms {
		if !seenRooms[room.RoomID] {
			seenRooms[room.RoomID] = true
			deduplicatedRooms = append(deduplicatedRooms, room)
		}
	}

	if len(unvisited) == 0 {
		// If no more rooms to walk, then don't return a walker for future pages
		return deduplicatedRooms, inaccessible, nil, nil
	} else {
		// If there are more rooms to walk, then return a new walker to resume walking from (for querying more pages)
		newWalker := roomserver.RoomHierarchyWalker{
			RootRoomID:    walker.RootRoomID,
			Caller:        walker.Caller,
			SuggestedOnly: walker.SuggestedOnly,
			MaxDepth:      walker.MaxDepth,
			RootVias:      walker.RootVias,
			RootRemote:    walker.RootRemote,
			Unvisited:     unvisited,
			Processed:     processed,
		}

		return deduplicatedRooms, inaccessible, &newWalker, nil
	}
}

// clientViaTimeout bounds the time spent asking the servers that a client
// named for a root room.
const clientViaTimeout = 30 * time.Second

// remoteRootAccessible returns the summary of a root room that this server
// is not in, from the servers the client named (MSC4235), if the user may see
// it. Those servers only check that this server may see the room, so the
// user's access is checked here. Nothing they return is cached, since the
// client chose them.
func remoteRootAccessible(ctx context.Context, querier *Queryer, walker roomserver.RoomHierarchyWalker) *fclient.RoomHierarchyResponse {
	device := walker.Caller.Device()
	if device == nil || len(walker.RootVias) == 0 || roomExists(ctx, querier, walker.RootRoomID) {
		return nil
	}
	userID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		return nil
	}
	res := federatedRoomInfo(ctx, querier, walker.Caller, walker.SuggestedOnly, walker.RootRoomID, nil, walker.RootVias, false)
	if res == nil {
		return nil
	}
	if !userMaySeeRoom(res.Room, userMembership(ctx, querier, *userID)) {
		return nil
	}
	return res
}

// userMaySeeRoom reports whether a user may see the summary of a room, here
// or on a remote server: the room is world readable, anyone may join or
// knock, the user is joined or invited, or the user is joined to one of the
// rooms that allow joining. The membership function returns the user's
// membership in a room, or "" if there is none.
func userMaySeeRoom(room fclient.RoomHierarchyRoom, membership func(roomID string) string) bool {
	if room.WorldReadable {
		return true
	}
	switch room.JoinRule {
	case spec.Public, spec.Knock, spec.KnockRestricted:
		return true
	}
	if m := membership(room.RoomID); m == spec.Join || m == spec.Invite {
		return true
	}
	if room.JoinRule != spec.Restricted {
		return false
	}
	for _, allowed := range room.AllowedRoomIDs {
		if membership(allowed) == spec.Join {
			return true
		}
	}
	return false
}

// authorized reports whether the caller may see the room, and returns the
// rooms whose members may join it without knocking.
func authorised(ctx context.Context, querier *Queryer, caller types.DeviceOrServerName, roomID spec.RoomID) (authed bool, resultAllowedRoomIDs []string) { //nolint:misspell
	if clientCaller := caller.Device(); clientCaller != nil {
		return authorisedUser(ctx, querier, clientCaller, roomID)
	}
	if serverCaller := caller.ServerName(); serverCaller != nil {
		return authorisedServer(ctx, querier, roomID, *serverCaller)
	}
	return false, nil
}

// authorisedServer returns true iff the server is joined this room or the room is world_readable, public, or knockable.
func authorisedServer(ctx context.Context, querier *Queryer, roomID spec.RoomID, callerServerName spec.ServerName) (bool, []string) {
	// Check history visibility / join rules first
	hisVisTuple := gomatrixserverlib.StateKeyTuple{
		EventType: spec.MRoomHistoryVisibility,
		StateKey:  "",
	}
	joinRuleTuple := gomatrixserverlib.StateKeyTuple{
		EventType: spec.MRoomJoinRules,
		StateKey:  "",
	}
	var queryRoomRes roomserver.QueryCurrentStateResponse
	err := querier.QueryCurrentState(ctx, &roomserver.QueryCurrentStateRequest{
		RoomID: roomID.String(),
		StateTuples: []gomatrixserverlib.StateKeyTuple{
			hisVisTuple, joinRuleTuple,
		},
	}, &queryRoomRes)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("failed to QueryCurrentState")
		return false, []string{}
	}
	hisVisEv := queryRoomRes.StateEvents[hisVisTuple]
	if hisVisEv != nil {
		hisVis, _ := hisVisEv.HistoryVisibility()
		if hisVis == "world_readable" {
			return true, []string{}
		}
	}

	// check if this room is a restricted room and if so, we need to check if the server is joined to an allowed room ID
	// in addition to the actual room ID (but always do the actual one first as it's quicker in the common case)
	allowJoinedToRoomIDs := []spec.RoomID{roomID}
	joinRuleEv := queryRoomRes.StateEvents[joinRuleTuple]

	if joinRuleEv != nil {
		rule, ruleErr := joinRuleEv.JoinRule()
		if ruleErr != nil {
			util.GetLogger(ctx).WithError(ruleErr).WithField("parent_room_id", roomID).Warn("failed to get join rule")
			return false, []string{}
		}

		if rule == spec.Public || rule == spec.Knock {
			return true, []string{}
		}

		if rule == spec.KnockRestricted {
			// Anyone may knock, so the room is accessible like a knock room,
			// and the allow list tells the caller who may join without knocking.
			allowed := []string{}
			for _, allowedRoomID := range restrictedJoinRuleAllowedRooms(ctx, joinRuleEv) {
				allowed = append(allowed, allowedRoomID.String())
			}
			return true, allowed
		}

		if rule == spec.Restricted {
			allowJoinedToRoomIDs = append(allowJoinedToRoomIDs, restrictedJoinRuleAllowedRooms(ctx, joinRuleEv)...)
		}
	}

	// check if server is joined to any allowed room
	resultAllowedRoomIDs := make([]string, 0, len(allowJoinedToRoomIDs))
	for _, allowedRoomID := range allowJoinedToRoomIDs {
		resultAllowedRoomIDs = append(resultAllowedRoomIDs, allowedRoomID.String())
	}
	for _, allowedRoomID := range allowJoinedToRoomIDs {
		var queryRes fs.QueryJoinedHostServerNamesInRoomResponse
		err = querier.FSAPI.QueryJoinedHostServerNamesInRoom(ctx, &fs.QueryJoinedHostServerNamesInRoomRequest{
			RoomID: allowedRoomID.String(),
		}, &queryRes)
		if err != nil {
			util.GetLogger(ctx).WithError(err).Error("failed to QueryJoinedHostServerNamesInRoom")
			continue
		}
		for _, srv := range queryRes.ServerNames {
			if srv == callerServerName {
				return true, resultAllowedRoomIDs[1:]
			}
		}
	}

	return false, resultAllowedRoomIDs[1:]
}

// authorisedUser reports whether the user may see the room, by the rules of
// userMaySeeRoom, and returns the rooms whose members may join it without
// knocking.
func authorisedUser(ctx context.Context, querier *Queryer, clientCaller *userapi.Device, roomID spec.RoomID) (authed bool, resultAllowedRoomIDs []string) {
	userID, err := spec.NewUserID(clientCaller.UserID, true)
	if err != nil {
		return false, nil
	}
	hisVisTuple := gomatrixserverlib.StateKeyTuple{
		EventType: spec.MRoomHistoryVisibility,
		StateKey:  "",
	}
	joinRuleTuple := gomatrixserverlib.StateKeyTuple{
		EventType: spec.MRoomJoinRules,
		StateKey:  "",
	}
	var queryRes roomserver.QueryCurrentStateResponse
	err = querier.QueryCurrentState(ctx, &roomserver.QueryCurrentStateRequest{
		RoomID:      roomID.String(),
		StateTuples: []gomatrixserverlib.StateKeyTuple{hisVisTuple, joinRuleTuple},
	}, &queryRes)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("failed to QueryCurrentState")
		return false, nil
	}
	room := fclient.RoomHierarchyRoom{PublicRoom: fclient.PublicRoom{RoomID: roomID.String()}}
	if hisVisEv := queryRes.StateEvents[hisVisTuple]; hisVisEv != nil {
		hisVis, _ := hisVisEv.HistoryVisibility()
		room.WorldReadable = hisVis == gomatrixserverlib.HistoryVisibilityWorldReadable
	}
	if joinRuleEv := queryRes.StateEvents[joinRuleTuple]; joinRuleEv != nil {
		rule, ruleErr := joinRuleEv.JoinRule()
		if ruleErr != nil {
			util.GetLogger(ctx).WithError(ruleErr).WithField("room_id", roomID.String()).Warn("failed to get join rule")
		}
		room.JoinRule = rule
		for _, allowed := range restrictedJoinRuleAllowedRooms(ctx, joinRuleEv) {
			room.AllowedRoomIDs = append(room.AllowedRoomIDs, allowed.String())
		}
	}
	return userMaySeeRoom(room, userMembership(ctx, querier, *userID)), room.AllowedRoomIDs
}

// userMembership returns a function that looks up the user's membership in a
// room, or "" if the user has never been in it. It reads the membership table,
// which a join updates before the room's state is complete (MSC3706).
func userMembership(ctx context.Context, querier *Queryer, userID spec.UserID) func(roomID string) string {
	return func(roomID string) string {
		var membership roomserver.QueryMembershipForUserResponse
		if err := querier.QueryMembershipForUser(ctx, &roomserver.QueryMembershipForUserRequest{
			RoomID: roomID, UserID: userID,
		}, &membership); err != nil || !membership.HasBeenInRoom {
			return ""
		}
		return membership.Membership
	}
}

// helper function to fetch a state event.
func stateEvent(ctx context.Context, querier *Queryer, roomID spec.RoomID, evType, stateKey string) *types.HeaderedEvent {
	var queryRes roomserver.QueryCurrentStateResponse
	tuple := gomatrixserverlib.StateKeyTuple{
		EventType: evType,
		StateKey:  stateKey,
	}
	err := querier.QueryCurrentState(ctx, &roomserver.QueryCurrentStateRequest{
		RoomID:      roomID.String(),
		StateTuples: []gomatrixserverlib.StateKeyTuple{tuple},
	}, &queryRes)
	if err != nil {
		return nil
	}
	return queryRes.StateEvents[tuple]
}

// returns true if the current server is participating in the provided room.
func roomExists(ctx context.Context, querier *Queryer, roomID spec.RoomID) bool {
	var queryRes roomserver.QueryServerJoinedToRoomResponse
	err := querier.QueryServerJoinedToRoom(ctx, &roomserver.QueryServerJoinedToRoomRequest{
		RoomID:     roomID.String(),
		ServerName: querier.Cfg.Global.ServerName,
	}, &queryRes)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("failed to QueryServerJoinedToRoom")
		return false
	}
	// if the room exists but we aren't in the room then we might have stale data so we want to fetch
	// it fresh via federation
	return queryRes.RoomExists && queryRes.IsInRoom
}

// federatedRoomInfo returns more of the spaces graph from another server. Returns nil if this was
// unsuccessful.
//
// The vias are trusted when they come from room state. Otherwise a client
// chose them (MSC4235): the servers are then asked within the request's
// lifetime, bounded by clientViaTimeout, and neither their answers nor their
// failures are cached, so that one user cannot change what others see.
func federatedRoomInfo(
	ctx context.Context, querier *Queryer, caller types.DeviceOrServerName, suggestedOnly bool, roomID spec.RoomID, parentRoomID *spec.RoomID, vias []string, trusted bool,
) *fclient.RoomHierarchyResponse {
	// only do federated requests for client requests
	if caller.Device() == nil {
		return nil
	}
	roomIDStr := roomID.String()
	resp, ok := querier.Cache.GetRoomHierarchy(roomIDStr)
	if ok {
		util.GetLogger(ctx).Debugf("Returning cached response for %s", roomID)
		return &resp
	}
	// Check negative cache - if we recently failed to fetch this room, skip federation
	var refusalKey string
	// Answers from trusted servers are cached for everyone, so a client
	// canceling its request must not stop them.
	innerCtx := context.Background() //nolint:contextcheck
	if trusted {
		refusalKey = roomHierarchyRefusalKey(ctx, querier, roomID, parentRoomID, vias)
		if querier.Cache.GetRoomHierarchyFailure(roomIDStr) || querier.Cache.GetRoomHierarchyFailure(refusalKey) {
			util.GetLogger(ctx).Debugf("Skipping federation for %s (recently failed)", roomID)
			return nil
		}
	} else {
		var cancel context.CancelFunc
		innerCtx, cancel = context.WithTimeout(ctx, clientViaTimeout)
		defer cancel()
	}
	util.GetLogger(ctx).Debugf("Querying %s via %+v", roomID, vias)
	refused, unreachable := false, false
	// query more of the spaces graph using these servers
	for _, serverName := range vias {
		if serverName == string(querier.Cfg.Global.ServerName) {
			continue
		}
		if innerCtx.Err() != nil {
			break
		}
		res, err := querier.FSAPI.RoomHierarchies(innerCtx, querier.Cfg.Global.ServerName, spec.ServerName(serverName), roomIDStr, suggestedOnly)
		if err != nil {
			var fedErr *fs.FederationClientError
			if errors.As(err, &fedErr) && fedErr.Code == http.StatusNotFound {
				util.GetLogger(ctx).WithError(err).Debugf("room %s not available via server %s", roomIDStr, serverName)
				refused = true
			} else {
				util.GetLogger(ctx).WithError(err).Warnf("failed to call RoomHierarchies on server %s", serverName)
				unreachable = true
			}
			continue
		}
		if res.Room.RoomID != roomIDStr {
			util.GetLogger(ctx).Warnf("server %s answered for room %q instead of %s", serverName, res.Room.RoomID, roomIDStr)
			unreachable = true
			continue
		}
		// ensure nil slices are empty as we send this to the client sometimes
		if res.Room.ChildrenState == nil {
			res.Room.ChildrenState = []fclient.RoomHierarchyStrippedEvent{}
		}
		for i := 0; i < len(res.Children); i++ {
			child := res.Children[i]
			if child.ChildrenState == nil {
				child.ChildrenState = []fclient.RoomHierarchyStrippedEvent{}
			}
			res.Children[i] = child
		}
		if trusted {
			querier.Cache.StoreRoomHierarchy(roomIDStr, res)
		}

		return &res
	}
	if !trusted {
		return nil
	}
	if refused && !unreachable {
		util.GetLogger(ctx).Debugf("The servers refused %s, caching negative result until one joins the parent", roomID)
		querier.Cache.StoreRoomHierarchyFailure(refusalKey)
		return nil
	}
	// All vias failed - cache this negative result to avoid repeated failed attempts
	util.GetLogger(ctx).Debugf("All federation attempts failed for %s, caching negative result", roomID)
	querier.Cache.StoreRoomHierarchyFailure(roomIDStr)
	return nil
}

// roomHierarchyRefusalKey is the cache key for the servers' refusal to share
// a room. A server that answers 404 does not know yet that a local user may
// join the room, which typically changes when it joins the parent space and
// learns its members, so the key holds the vias that are in the parent.
func roomHierarchyRefusalKey(ctx context.Context, querier *Queryer, roomID spec.RoomID, parentRoomID *spec.RoomID, vias []string) string {
	key := roomID.String() + "|refused"
	if parentRoomID == nil {
		return key
	}
	key += "|" + parentRoomID.String()
	joined := make([]string, 0, len(vias))
	for _, via := range vias {
		var res roomserver.QueryServerJoinedToRoomResponse
		err := querier.QueryServerJoinedToRoom(ctx, &roomserver.QueryServerJoinedToRoomRequest{
			RoomID: parentRoomID.String(), ServerName: spec.ServerName(via),
		}, &res)
		if err == nil && res.IsInRoom {
			joined = append(joined, via)
		}
	}
	sort.Strings(joined)
	return key + "|" + strings.Join(joined, ",")
}

// references returns all child references pointing to or from this room.
func childReferences(ctx context.Context, querier *Queryer, suggestedOnly bool, roomID spec.RoomID) ([]fclient.RoomHierarchyStrippedEvent, error) {
	createTuple := gomatrixserverlib.StateKeyTuple{
		EventType: spec.MRoomCreate,
		StateKey:  "",
	}
	var res roomserver.QueryCurrentStateResponse
	err := querier.QueryCurrentState(context.Background(), &roomserver.QueryCurrentStateRequest{ //nolint:contextcheck
		RoomID:         roomID.String(),
		AllowWildcards: true,
		StateTuples: []gomatrixserverlib.StateKeyTuple{
			createTuple, {
				EventType: spec.MSpaceChild,
				StateKey:  "*",
			},
		},
	}, &res)
	if err != nil {
		return nil, err
	}

	// don't return any child refs if the room is not a space room
	if create := res.StateEvents[createTuple]; create != nil {
		var createContent gomatrixserverlib.CreateContent
		err := json.Unmarshal(create.Content(), &createContent)
		if err != nil {
			util.GetLogger(ctx).WithError(err).WithField("create_content", create.Content()).Warn("failed to unmarshal m.room.create event")
		}
		roomType := createContent.RoomType
		if roomType != spec.MSpace {
			return []fclient.RoomHierarchyStrippedEvent{}, nil
		}
	}
	delete(res.StateEvents, createTuple)

	el := make([]fclient.RoomHierarchyStrippedEvent, 0, len(res.StateEvents))
	for _, ev := range res.StateEvents {
		content := gjson.ParseBytes(ev.Content())
		// only return events that have a `via` key as per MSC1772
		// else we'll incorrectly walk redacted events (as the link
		// is in the state_key)
		if content.Get("via").Exists() {
			strip := stripped(ev.PDU)
			if strip == nil {
				continue
			}
			// if suggested only and this child isn't suggested, skip it.
			// if suggested only = false we include everything so don't need to check the content.
			if suggestedOnly && !content.Get("suggested").Bool() {
				continue
			}
			el = append(el, *strip)
		}
	}
	// sort by origin_server_ts as per MSC2946
	sort.Slice(el, func(i, j int) bool {
		return el[i].OriginServerTS < el[j].OriginServerTS
	})

	return el, nil
}

// fetch public room information for provided room.
func publicRoomsChunk(ctx context.Context, querier *Queryer, roomID spec.RoomID) *fclient.PublicRoom {
	pubRooms, err := roomserver.PopulatePublicRooms(ctx, []string{roomID.String()}, querier)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("failed to PopulatePublicRooms")
		return nil
	}
	if len(pubRooms) == 0 {
		return nil
	}
	return &pubRooms[0].PublicRoom
}

func stripped(ev gomatrixserverlib.PDU) *fclient.RoomHierarchyStrippedEvent {
	if ev.StateKey() == nil {
		return nil
	}
	return &fclient.RoomHierarchyStrippedEvent{
		Type:           ev.Type(),
		StateKey:       *ev.StateKey(),
		Content:        ev.Content(),
		Sender:         string(ev.SenderID()),
		OriginServerTS: ev.OriginServerTS(),
	}
}

// given a restricted or knock_restricted join_rule event, return list of rooms where membership of that room allows joining.
func restrictedJoinRuleAllowedRooms(ctx context.Context, joinRuleEv *types.HeaderedEvent) (allows []spec.RoomID) {
	rule, _ := joinRuleEv.JoinRule()
	if rule != spec.Restricted && rule != spec.KnockRestricted {
		return nil
	}
	var jrContent gomatrixserverlib.JoinRuleContent
	if err := json.Unmarshal(joinRuleEv.Content(), &jrContent); err != nil {
		util.GetLogger(ctx).Warnf("failed to check join_rule on room %s: %s", joinRuleEv.RoomID().String(), err)
		return nil
	}
	for _, allow := range jrContent.Allow {
		if allow.Type == spec.MRoomMembership {
			allowedRoomID, err := spec.NewRoomID(allow.RoomID)
			if err != nil {
				util.GetLogger(ctx).Warnf("invalid room ID '%s' found in join_rule on room %s: %s", allow.RoomID, joinRuleEv.RoomID().String(), err)
			} else {
				allows = append(allows, *allowedRoomID)
			}
		}
	}
	return
}
