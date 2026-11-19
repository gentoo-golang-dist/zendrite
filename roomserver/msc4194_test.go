// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/test"
)

// TestQueryUnredactedEventsBySender checks the search for a sender's events
// that MSC4194 redacts: newest first, across more than one batch of
// candidates, without events of others that mention the sender, and without
// events that are already redacted.
func TestQueryUnredactedEventsBySender(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		rsAPI, db := redactedContentHarness(t, dbType, 0)
		ctx := context.Background()

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		join := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
		bobEvents := []string{join.EventID()}
		roomInfo, err := db.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		messageTypeNID, err := db.GetOrCreateEventTypeNID(ctx, "m.room.message")
		require.NoError(t, err)
		// Bob's messages are spread over more than one batch of 500
		// candidates, between messages of alice that mention him and so
		// contain his ID. Alice's messages are written to the database
		// directly, where the search reads them like any other event, since
		// processing over a thousand events takes minutes under the race
		// detector on CI.
		for i := range 6 {
			msg := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": fmt.Sprint(i)})
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{msg}, "test", "test", "test", nil, false))
			bobEvents = append(bobEvents, msg.EventID())
			for j := range 199 {
				mention := room.CreateEvent(t, alice, "m.room.message", map[string]any{
					"msgtype": "m.text", "body": fmt.Sprintf("ping %s %d/%d", bob.ID, i, j), "sender": bob.ID,
				})
				_, _, err = db.StoreEvent(ctx, mention.PDU, roomInfo, messageTypeNID, 0, nil, false, true)
				require.NoError(t, err)
			}
		}
		redaction := room.CreateAndInsert(t, alice, spec.MRoomRedaction, map[string]any{}, test.WithRedacts(bobEvents[1]))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{redaction}, "test", "test", "test", nil, false))
		roomID, err := spec.NewRoomID(room.ID)
		require.NoError(t, err)

		slices.Reverse(bobEvents)
		// Bob's first message, second to last when newest come first, is redacted.
		want := slices.Delete(slices.Clone(bobEvents), len(bobEvents)-2, len(bobEvents)-1)
		ids := func(limit int) []string {
			t.Helper()
			events, queryErr := rsAPI.QueryUnredactedEventsBySender(ctx, *roomID, spec.SenderID(bob.ID), limit)
			require.NoError(t, queryErr)
			var got []string
			for _, ev := range events {
				require.False(t, ev.Undelivered, ev.EventID)
				got = append(got, ev.EventID)
			}
			return got
		}
		require.Equal(t, want, ids(100))
		require.Equal(t, want[:3], ids(3))
		require.Empty(t, ids(0))

		events, err := rsAPI.QueryUnredactedEventsBySender(ctx, *roomID, "@nobody:test", 10)
		require.NoError(t, err)
		require.Empty(t, events)
		unknownRoom, err := spec.NewRoomID("!unknown:test")
		require.NoError(t, err)
		events, err = rsAPI.QueryUnredactedEventsBySender(ctx, *unknownRoom, spec.SenderID(bob.ID), 10)
		require.NoError(t, err)
		require.Empty(t, events)
	})
}
