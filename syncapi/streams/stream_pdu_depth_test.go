// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package streams

import (
	"encoding/json"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/stretchr/testify/require"

	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

func eventAtDepth(t *testing.T, body string, depth int64) types.StreamEvent {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "m.room.message", "room_id": "!a:test", "sender": "@a:test", "depth": depth,
		"origin_server_ts": 1, "prev_events": []string{}, "auth_events": []string{},
		"content": map[string]any{"body": body},
	})
	require.NoError(t, err)
	ev, err := gomatrixserverlib.MustGetRoomVersion(gomatrixserverlib.RoomVersionV10).NewEventFromTrustedJSONWithEventID("$"+body, raw, false)
	require.NoError(t, err)
	return types.StreamEvent{HeaderedEvent: &rstypes.HeaderedEvent{PDU: ev}}
}

func TestLatestByDepth(t *testing.T) {
	e3 := eventAtDepth(t, "e3", 3)
	e4 := eventAtDepth(t, "e4", 4)
	// Arrived after the others, but forks off before them.
	s2 := eventAtDepth(t, "s2", 3)

	recent := types.RecentEvents{Events: []types.StreamEvent{e3, e4, s2}}

	timeline := latestByDepth(recent, 1)
	require.Equal(t, []string{"$e4"}, eventIDs(timeline))
	require.True(t, timeline.Limited)

	timeline = latestByDepth(recent, 2)
	require.Equal(t, []string{"$s2", "$e4"}, eventIDs(timeline), "stream order must break ties in depth")
	require.True(t, timeline.Limited)

	timeline = latestByDepth(recent, 3)
	require.Equal(t, []string{"$e3", "$s2", "$e4"}, eventIDs(timeline))
	require.False(t, timeline.Limited)
	require.Equal(t, []string{"$e3", "$e4", "$s2"}, eventIDs(recent), "the given events were changed")

	timeline = latestByDepth(recent, 0)
	require.Empty(t, timeline.Events)
}

func eventIDs(recent types.RecentEvents) []string {
	ids := make([]string, 0, len(recent.Events))
	for _, ev := range recent.Events {
		ids = append(ids, ev.EventID())
	}
	return ids
}
