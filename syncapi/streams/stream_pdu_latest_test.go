// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package streams

import (
	"testing"

	"github.com/stretchr/testify/require"

	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
)

func TestLatestDelivered(t *testing.T) {
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	message := func() *rstypes.HeaderedEvent {
		return room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "hi"})
	}
	late, older, newer, newest, hidden := message(), message(), message(), message(), message()

	// The late event sorts first by the DAG, but arrived after the older event.
	dagOrder := []*rstypes.HeaderedEvent{late, older, newer, newest}
	var stream []types.StreamEvent
	for _, ev := range []*rstypes.HeaderedEvent{older, late, newer, hidden, newest} {
		stream = append(stream, types.StreamEvent{HeaderedEvent: ev})
	}

	got := latestDelivered(dagOrder, stream, 3)
	require.Equal(t, []string{late.EventID(), newer.EventID(), newest.EventID()}, []string{got[0].EventID(), got[1].EventID(), got[2].EventID()})
	require.Len(t, got, 3, "an event history visibility hid took up a place")
}
