// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared

import (
	"slices"
	"testing"

	"github.com/matrix-org/util"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/roomserver/types"
)

func TestWithBase(t *testing.T) {
	for _, tc := range []struct {
		name        string
		blocks      []types.StateBlockNID
		base, block types.StateBlockNID
		want        []types.StateBlockNID
		adjacent    bool
	}{
		{"first block", []types.StateBlockNID{10, 20}, 5, 10, []types.StateBlockNID{5, 10, 20}, true},
		{"base already there", []types.StateBlockNID{5, 10, 20}, 5, 10, []types.StateBlockNID{5, 10, 20}, true},
		{"block between", []types.StateBlockNID{7, 10}, 5, 10, []types.StateBlockNID{5, 7, 10}, false},
		{"earlier block before base", []types.StateBlockNID{3, 10}, 5, 10, []types.StateBlockNID{3, 5, 10}, true},
		{"base after block", []types.StateBlockNID{10}, 15, 10, []types.StateBlockNID{10, 15}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, adjacent := withBase(tc.blocks, tc.base, tc.block)
			require.Equal(t, types.StateBlockNIDs(tc.want), got)
			require.Equal(t, tc.adjacent, adjacent)
		})
	}
}

func block(entries ...types.StateEntry) *compactionBlock {
	b := &compactionBlock{state: map[types.StateKeyTuple]types.EventNID{}}
	for _, e := range entries {
		b.state[e.StateKeyTuple] = e.EventNID
	}
	return b
}

func entry(key types.EventStateKeyNID, event types.EventNID) types.StateEntry {
	return types.StateEntry{StateKeyTuple: types.StateKeyTuple{EventTypeNID: types.MRoomMemberNID, EventStateKeyNID: key}, EventNID: event}
}

func TestCompactionDelta(t *testing.T) {
	base := block(entry(1, 10), entry(2, 20), entry(3, 30))
	delta, ok := compactionDelta(base, block(entry(1, 10), entry(2, 21), entry(3, 30), entry(4, 40)))
	require.True(t, ok)
	slices.SortFunc(delta, func(a, b types.StateEntry) int { return int(a.EventNID) - int(b.EventNID) })
	require.Equal(t, types.StateEntries{entry(2, 21), entry(4, 40)}, delta)

	_, ok = compactionDelta(base, block(entry(1, 10), entry(2, 20)))
	require.False(t, ok, "a block without a key of the base")
}

// The rewritten events must be stored, and hashed, as BulkInsertStateData
// stores a block with the same entries.
func TestStoredEventsMatchesInsertOrder(t *testing.T) {
	entries := types.StateEntries{entry(5, 7), entry(1, 90), entry(3, 12), entry(1, 90)}
	inserted := slices.Clone(entries)
	inserted = inserted[:util.SortAndUnique(inserted)]
	want := make(types.EventNIDs, len(inserted))
	for i, e := range inserted {
		want[i] = e.EventNID
	}
	got := storedEvents(entries)
	require.Equal(t, want, got)
	require.Equal(t, want.Hash(), got.Hash())
}
