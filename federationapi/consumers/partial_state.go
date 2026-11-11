// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"context"
	"slices"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/roomserver/api"
)

// withPartialStateServers adds, to the servers in the given rooms, the
// servers the resident server named when one of them was joined with partial
// state: while a room has partial state, this server does not know who is in
// it, and approximates the servers in it with those, as Synapse does
// (MSC3902). Local servers are left out.
func withPartialStateServers(
	ctx context.Context, rsAPI api.FederationRoomserverAPI, isLocal func(spec.ServerName) bool,
	roomIDs []string, servers []spec.ServerName,
) []spec.ServerName {
	for _, roomID := range roomIDs {
		validRoomID, err := spec.NewRoomID(roomID)
		if err != nil {
			continue
		}
		roomInfo, err := rsAPI.QueryRoomInfo(ctx, *validRoomID)
		if err != nil || roomInfo == nil {
			continue
		}
		partial, err := rsAPI.IsRoomPartialState(ctx, roomInfo.RoomNID)
		if err != nil || !partial {
			continue
		}
		partialServers, err := rsAPI.GetPartialStateServers(ctx, roomInfo.RoomNID)
		if err != nil {
			logrus.WithError(err).WithField("room_id", roomID).Warn("Failed to get the servers of a partial state room")
			continue
		}
		for _, s := range partialServers {
			server := spec.ServerName(s)
			if isLocal(server) || slices.Contains(servers, server) {
				continue
			}
			servers = append(servers, server)
		}
	}
	return servers
}
