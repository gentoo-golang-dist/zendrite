// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/test"
)

// TestQueryEventOrdering checks the event positions the client API uses to
// keep m.fully_read moving forward (MSC4446): depth first, then the order in
// which the roomserver stored events of the same depth.
func TestQueryEventOrdering(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		rsAPI, _ := redactedContentHarness(t, dbType, 0)
		ctx := context.Background()

		room := test.NewRoom(t, alice)
		older := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "1"})
		// Two events at the same depth, stored in this order.
		forkA := room.CreateEvent(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "a"})
		forkB := room.CreateEvent(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "b"})
		room.InsertEvent(t, forkA)
		room.InsertEvent(t, forkB)
		other := test.NewRoom(t, alice)
		otherEvent := other.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "x"})
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, other.Events(), "test", "test", "test", nil, false))

		orderings, err := rsAPI.QueryEventOrdering(ctx, room.ID, []string{older.EventID(), forkA.EventID(), forkB.EventID(), otherEvent.EventID(), "$unknown:test"})
		require.NoError(t, err)
		require.Len(t, orderings, 3)
		require.Equal(t, forkA.Depth(), forkB.Depth())
		require.True(t, orderings[older.EventID()].Before(orderings[forkA.EventID()]))
		require.True(t, orderings[forkA.EventID()].Before(orderings[forkB.EventID()]))
		require.False(t, orderings[forkB.EventID()].Before(orderings[forkA.EventID()]))
		require.False(t, orderings[forkA.EventID()].Before(orderings[forkA.EventID()]))

		orderings, err = rsAPI.QueryEventOrdering(ctx, "!unknown:test", []string{older.EventID()})
		require.NoError(t, err)
		require.Empty(t, orderings)
		orderings, err = rsAPI.QueryEventOrdering(ctx, room.ID, nil)
		require.NoError(t, err)
		require.Empty(t, orderings)
	})
}
