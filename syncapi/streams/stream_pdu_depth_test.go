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
