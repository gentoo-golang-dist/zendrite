// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"encoding/json"
	"fmt"
	"slices"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	log "github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// sendDeviceLists tells the servers of a room a local user has just joined
// about the user's devices, unless they share another room with the user and
// so already receive their device list updates.
func (s *OutputRoomEventConsumer) sendDeviceLists(ore api.OutputNewRoomEvent) {
	if s.userAPI == nil {
		return
	}
	users := s.userAPI()
	if users == nil {
		return
	}
	userID, joined, err := s.newlyJoinedLocalUser(ore)
	if err != nil {
		log.WithError(err).WithField("event_id", ore.Event.EventID()).Warn("Failed to tell whether a local user joined")
		return
	}
	if !joined {
		return
	}
	logger := log.WithField("user_id", userID.String()).WithField("room_id", ore.Event.RoomID().String())

	// A join with partial state does not announce the user's devices, as
	// Synapse does not: the resident servers learn about them from the
	// user's next device change (MSC3902).
	if roomID, rerr := spec.NewRoomID(ore.Event.RoomID().String()); rerr == nil {
		if roomInfo, ierr := s.rsAPI.QueryRoomInfo(s.ctx, *roomID); ierr == nil && roomInfo != nil {
			if partial, perr := s.rsAPI.IsRoomPartialState(s.ctx, roomInfo.RoomNID); perr == nil && partial {
				return
			}
		}
	}

	destinations, err := s.serversNewToUser(ore.Event.RoomID().String(), userID)
	if err != nil {
		logger.WithError(err).Warn("Failed to work out which servers to send a device list to")
		return
	}
	if len(destinations) == 0 {
		return
	}
	s.sendDeviceListsTo(userID, destinations)
}

// sendDeviceListsTo sends the user's device list to the servers, one update
// per device.
func (s *OutputRoomEventConsumer) sendDeviceListsTo(userID spec.UserID, destinations []spec.ServerName) {
	if s.userAPI == nil {
		return
	}
	users := s.userAPI()
	if users == nil {
		return
	}
	logger := log.WithField("user_id", userID.String())
	var err error
	var devices userapi.QueryDevicesResponse
	if err = users.QueryDevices(s.ctx, &userapi.QueryDevicesRequest{UserID: userID.String()}, &devices); err != nil {
		logger.WithError(err).Warn("Failed to query the devices of a user who joined")
		return
	}
	var keys userapi.QueryDeviceMessagesResponse
	if err = users.QueryDeviceMessages(s.ctx, &userapi.QueryDeviceMessagesRequest{UserID: userID.String()}, &keys); err != nil {
		logger.WithError(err).Warn("Failed to query the device keys of a user who joined")
		return
	}
	if keys.Error != nil {
		logger.WithError(keys.Error).Warn("Failed to query the device keys of a user who joined")
		return
	}

	for _, update := range deviceListUpdates(userID.String(), devices.Devices, keys) {
		edu := &gomatrixserverlib.EDU{Type: spec.MDeviceListUpdate, Origin: string(userID.Domain())}
		if edu.Content, err = json.Marshal(update); err != nil {
			logger.WithError(err).Error("Failed to marshal a device list update")
			return
		}
		if err = s.queues.SendEDU(edu, userID.Domain(), destinations); err != nil {
			logger.WithError(err).Warn("Failed to send a device list update")
			return
		}
	}
}

// newlyJoinedLocalUser returns the local user the event joins to the room. A
// member event of someone already joined, such as a new display name, changes
// no one's room list and returns false.
func (s *OutputRoomEventConsumer) newlyJoinedLocalUser(ore api.OutputNewRoomEvent) (spec.UserID, bool, error) {
	if ore.Event.Type() != spec.MRoomMember || ore.Event.StateKey() == nil {
		return spec.UserID{}, false, nil
	}
	if membership, err := ore.Event.Membership(); err != nil || membership != spec.Join {
		return spec.UserID{}, false, nil //nolint:nilerr // a malformed member event joins no one
	}
	roomID := ore.Event.RoomID()
	userID, err := s.rsAPI.QueryUserIDForSender(s.ctx, roomID, spec.SenderID(*ore.Event.StateKey()))
	if err != nil {
		return spec.UserID{}, false, fmt.Errorf("s.rsAPI.QueryUserIDForSender: %w", err)
	}
	if userID == nil || !s.cfg.Matrix.IsLocalServerName(userID.Domain()) {
		return spec.UserID{}, false, nil
	}
	if len(ore.RemovesStateEventIDs) > 0 {
		var removed api.QueryEventsByIDResponse
		if err = s.rsAPI.QueryEventsByID(s.ctx, &api.QueryEventsByIDRequest{
			RoomID: roomID.String(), EventIDs: ore.RemovesStateEventIDs,
		}, &removed); err != nil {
			return spec.UserID{}, false, fmt.Errorf("s.rsAPI.QueryEventsByID: %w", err)
		}
		for _, ev := range removed.Events {
			if ev.Type() != spec.MRoomMember || !ev.StateKeyEquals(*ore.Event.StateKey()) {
				continue
			}
			if membership, _ := ev.Membership(); membership == spec.Join {
				return spec.UserID{}, false, nil
			}
		}
	}
	return *userID, true, nil
}

// serversNewToUser returns the servers in the room that share no other room
// with the user.
func (s *OutputRoomEventConsumer) serversNewToUser(roomID string, userID spec.UserID) ([]spec.ServerName, error) {
	joinedHosts, err := s.db.GetJoinedHosts(s.ctx, roomID)
	if err != nil {
		return nil, fmt.Errorf("s.db.GetJoinedHosts: %w", err)
	}
	hosts := make([]spec.ServerName, 0, len(joinedHosts))
	for _, host := range joinedHosts {
		hosts = append(hosts, host.ServerName)
	}

	rooms, err := s.rsAPI.QueryRoomsForUser(s.ctx, userID, spec.Join)
	if err != nil {
		return nil, fmt.Errorf("s.rsAPI.QueryRoomsForUser: %w", err)
	}
	otherRooms := make([]string, 0, len(rooms))
	for _, room := range rooms {
		if room.String() != roomID {
			otherRooms = append(otherRooms, room.String())
		}
	}
	if len(otherRooms) == 0 {
		return hosts, nil
	}
	known, err := s.db.GetJoinedHostsForRooms(s.ctx, otherRooms, true, true)
	if err != nil {
		return nil, fmt.Errorf("s.db.GetJoinedHostsForRooms: %w", err)
	}
	return withoutServers(hosts, known), nil
}

func withoutServers(servers, excluded []spec.ServerName) []spec.ServerName {
	remaining := make([]spec.ServerName, 0, len(servers))
	for _, server := range servers {
		if !slices.Contains(excluded, server) {
			remaining = append(remaining, server)
		}
	}
	return remaining
}

// deviceListUpdates returns one update per device. A device without keys is
// still part of the device list, so it is not reported as deleted.
func deviceListUpdates(
	userID string, devices []userapi.Device, keys userapi.QueryDeviceMessagesResponse,
) []gomatrixserverlib.DeviceListUpdateEvent {
	keyed := make(map[string]userapi.DeviceMessage, len(keys.Devices))
	for _, device := range keys.Devices {
		keyed[device.DeviceID] = device
	}
	updates := make([]gomatrixserverlib.DeviceListUpdateEvent, 0, len(devices))
	for _, device := range devices {
		update := gomatrixserverlib.DeviceListUpdateEvent{
			UserID:            userID,
			DeviceID:          device.ID,
			DeviceDisplayName: device.DisplayName,
			StreamID:          keys.StreamID,
			PrevID:            prevID(keys.StreamID),
		}
		if key, ok := keyed[device.ID]; ok {
			update.StreamID = key.StreamID
			update.PrevID = prevID(key.StreamID)
			update.Keys = key.KeyJSON
		}
		updates = append(updates, update)
	}
	return updates
}

// onUnPartialStatedRoom brings the servers in a room that got its full state
// after a partial-state join up to date (MSC3902). The joined hosts become
// those of the full state, and the servers that the device list updates of
// local users, sent during the partial state, may have missed are sent the
// current device lists, as Synapse does.
func (s *OutputRoomEventConsumer) onUnPartialStatedRoom(msg api.OutputUnPartialStatedRoom) error {
	var stateRes api.QueryLatestEventsAndStateResponse
	if err := s.rsAPI.QueryLatestEventsAndState(s.ctx, &api.QueryLatestEventsAndStateRequest{
		RoomID:       msg.RoomID,
		StateToFetch: nil,
	}, &stateRes); err != nil {
		return fmt.Errorf("s.rsAPI.QueryLatestEventsAndState: %w", err)
	}
	if stateRes.RoomExists {
		members := make([]gomatrixserverlib.PDU, 0, len(stateRes.StateEvents))
		for _, ev := range stateRes.StateEvents {
			if ev.Type() == spec.MRoomMember {
				members = append(members, ev.PDU)
			}
		}
		joinedHosts, err := JoinedHostsFromEvents(s.ctx, members, s.rsAPI)
		if err != nil {
			return fmt.Errorf("JoinedHostsFromEvents: %w", err)
		}
		if _, err = s.db.UpdateRoom(s.ctx, msg.RoomID, joinedHosts, nil, true); err != nil {
			return fmt.Errorf("s.db.UpdateRoom: %w", err)
		}
	}
	if len(msg.DeviceListServers) == 0 {
		return nil
	}
	destinations := make([]spec.ServerName, 0, len(msg.DeviceListServers))
	for _, server := range msg.DeviceListServers {
		destinations = append(destinations, spec.ServerName(server))
	}
	for _, rawUserID := range msg.JoinedUserIDs {
		userID, err := spec.NewUserID(rawUserID, true)
		if err != nil || !s.cfg.Matrix.IsLocalServerName(userID.Domain()) {
			continue
		}
		s.sendDeviceListsTo(*userID, destinations)
	}
	return nil
}
