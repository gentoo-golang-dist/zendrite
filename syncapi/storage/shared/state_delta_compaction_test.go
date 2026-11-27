// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared_test

import (
	"slices"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// Compacting the state deltas of a room only drops state from an incremental
// sync that the client already had or that did not exist yet, and a filtered
// sync stays the unfiltered one restricted to the filter.
func TestCompactStateDeltasKeepsIncrementalSyncs(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, cleanup := newSyncDB(t, dbType)
		defer cleanup()
		ctx := t.Context()
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		room := test.NewRoom(t, alice)
		var positions []types.StreamPosition
		firstAdd := map[string]types.StreamPosition{}
		memberEvents := map[string]bool{}
		write := func(event *rstypes.HeaderedEvent, adds []*rstypes.HeaderedEvent, remove []string) {
			ids := make([]string, 0, len(adds))
			for _, added := range adds {
				ids = append(ids, added.EventID())
			}
			pos, err := db.WriteEvent(ctx, event, adds, ids, remove, nil, false, gomatrixserverlib.HistoryVisibilityShared)
			require.NoError(t, err)
			positions = append(positions, pos)
			if event.Type() == spec.MRoomMember {
				memberEvents[room.ID+" "+event.EventID()] = true
			}
			for _, id := range ids {
				if _, ok := firstAdd[room.ID+" "+id]; !ok {
					firstAdd[room.ID+" "+id] = pos
				}
			}
		}

		var state []*rstypes.HeaderedEvent
		for _, ev := range room.Events() {
			write(ev, []*rstypes.HeaderedEvent{ev}, nil)
			state = append(state, ev)
		}
		topic := room.CreateEvent(t, alice, spec.MRoomTopic, map[string]any{"topic": "t"}, test.WithStateKey(""))
		write(topic, []*rstypes.HeaderedEvent{topic}, nil)
		state = append(state, topic)

		// A gap fill before #527 re-added the whole state of the room.
		gap := room.CreateEvent(t, alice, "m.room.message", map[string]any{"body": "gap"})
		write(gap, state, nil)

		// Bob's join arrives only as part of another row's state.
		bobJoin := room.CreateEvent(t, bob, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(bob.ID))
		carrier := room.CreateEvent(t, alice, "m.room.message", map[string]any{"body": "carrier"})
		write(carrier, []*rstypes.HeaderedEvent{bobJoin}, nil)
		memberEvents[room.ID+" "+bobJoin.EventID()] = true
		state = append(state, bobJoin)
		gap2 := room.CreateEvent(t, alice, "m.room.message", map[string]any{"body": "gap 2"})
		write(gap2, state, nil)

		// A row that removes and re-adds the topic keeps both.
		both := room.CreateEvent(t, alice, "m.room.message", map[string]any{"body": "both"})
		write(both, []*rstypes.HeaderedEvent{topic}, []string{topic.EventID()})

		before := allStateDeltas(t, db, alice.ID, positions)

		maxID := positions[len(positions)-1]
		owns := func() bool { return true }
		rows, ids, err := db.CompactStateDeltas(ctx, room.ID, maxID, owns)
		require.NoError(t, err)
		require.Equal(t, 2, rows)
		// Both gaps drop every ID: the first gap the state before Bob's
		// join, the second the whole state.
		require.Equal(t, (len(state)-1)+len(state), ids)

		rows, ids, err = db.CompactStateDeltas(ctx, room.ID, maxID, owns)
		require.NoError(t, err)
		require.Zero(t, rows+ids, "a second pass changes nothing")

		after := allStateDeltas(t, db, alice.ID, positions)
		var dropped int
		for r, ids := range before {
			for _, id := range ids {
				if slices.Contains(after[r], id) {
					continue
				}
				dropped++
				// Before compaction a gap row that re-added the user's own
				// membership made the sync treat the room as rejoined and
				// send its whole current state, including later state.
				pos, ok := firstAdd[id]
				require.True(t, ok, "sync %v lost %s, which no row added", r, id)
				require.True(t, pos <= r[0] || pos > r[1], "sync %v lost %s, first added at %d inside it", r, id, pos)
			}
			for _, id := range after[r] {
				require.Contains(t, ids, id, "sync %v gained %s", r, id)
			}
		}
		require.Positive(t, dropped, "compaction changed no sync")

		members := allStateDeltasFiltered(t, db, alice.ID, positions, []string{spec.MRoomMember})
		for r, ids := range after {
			var want []string
			for _, id := range ids {
				if memberEvents[id] {
					want = append(want, id)
				}
			}
			require.Equal(t, want, members[r], "the filtered sync %v is the unfiltered one restricted to members", r)
		}
	})
}

// allStateDeltas returns the state event IDs of the incremental syncs between
// every pair of positions.
func allStateDeltas(t *testing.T, db storage.Database, userID string, positions []types.StreamPosition) map[[2]types.StreamPosition][]string {
	t.Helper()
	return allStateDeltasFiltered(t, db, userID, positions, nil)
}

// allStateDeltasFiltered is allStateDeltas for syncs whose state filter keeps
// only the given event types, or all of them when types is nil.
func allStateDeltasFiltered(t *testing.T, db storage.Database, userID string, positions []types.StreamPosition, eventTypes []string) map[[2]types.StreamPosition][]string {
	t.Helper()
	ctx := t.Context()
	snapshot, err := db.NewDatabaseSnapshot(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, snapshot.Rollback()) }()
	device := &userapi.Device{UserID: userID, ID: "test"}
	filter := synctypes.DefaultStateFilter()
	if eventTypes != nil {
		filter.Types = &eventTypes
	}
	out := map[[2]types.StreamPosition][]string{}
	for i, from := range positions {
		for _, to := range positions[i+1:] {
			deltas, _, err := snapshot.GetStateDeltas(ctx, device, types.Range{From: from, To: to}, userID, &filter, &historicalStateAPI{})
			require.NoError(t, err)
			var ids []string
			for _, delta := range deltas {
				for _, ev := range delta.StateEvents {
					ids = append(ids, delta.RoomID+" "+ev.EventID())
				}
			}
			slices.Sort(ids)
			out[[2]types.StreamPosition{from, to}] = ids
		}
	}
	return out
}
