// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package streams

import (
	"context"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// pendingPartialStateSnapshot serves a room whose partial-state join the
// sync API has not settled yet, while the roomserver already has its full
// state.
type pendingPartialStateSnapshot struct {
	failoverSnapshot
	pending []string
	gotTo   *types.StreamPosition
}

func (s pendingPartialStateSnapshot) PartialStateRooms(_ context.Context, _ string, to types.StreamPosition) ([]string, error) {
	*s.gotTo = to
	return s.pending, nil
}

func (s pendingPartialStateSnapshot) UnPartialStatedRoomsInRange(context.Context, string, types.Range) ([]string, error) {
	return nil, nil
}

// An eager sync must leave a room joined with partial state out until the
// sync API has written its un-partial-stated records, even once the roomserver
// has the full state: otherwise the records send the room a second time as
// newly joined, with all its members in device_lists.changed again (MSC3706).
func TestIncrementalSyncLeavesOutPendingPartialStateJoins(t *testing.T) {
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	message := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "hello"})
	var gotTo types.StreamPosition
	snapshot := pendingPartialStateSnapshot{
		failoverSnapshot: failoverSnapshot{roomID: room.ID, events: []*rstypes.HeaderedEvent{message}},
		pending:          []string{room.ID},
		gotTo:            &gotTo,
	}
	provider := &PDUStreamProvider{rsAPI: noPartialStateRooms{}}
	req := &types.SyncRequest{
		Context:           t.Context(),
		Device:            &userapi.Device{UserID: alice.ID},
		Rooms:             map[string]string{},
		Response:          types.NewResponse(),
		Filter:            synctypes.DefaultFilter(),
		Log:               logrus.NewEntry(logrus.StandardLogger()),
		MembershipChanges: map[string]struct{}{},
	}

	from, to := types.StreamPosition(10), types.StreamPosition(20)
	require.Equal(t, to, provider.IncrementalSync(t.Context(), snapshot, req, from, to))
	assert.Equal(t, to, gotTo, "the rooms are those still partial up to the end of the range")
	assert.NotContains(t, req.Response.Rooms.Join, room.ID)
	assert.NotContains(t, req.Rooms, room.ID)
}
