// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package state

import (
	"context"
	"slices"
	"sort"
	"testing"

	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// memStateStorage keeps state blocks and snapshots in memory, appending
// blocks in increasing NID order as the database does.
type memStateStorage struct {
	StateResolutionStorage
	blocks    map[types.StateBlockNID][]types.StateEntry
	snapshots map[types.StateSnapshotNID][]types.StateBlockNID
	fullAdds  int
	deltaAdds int
}

func newMemStateStorage() *memStateStorage {
	return &memStateStorage{
		blocks:    map[types.StateBlockNID][]types.StateEntry{},
		snapshots: map[types.StateSnapshotNID][]types.StateBlockNID{},
	}
}

func (m *memStateStorage) StateBlockNIDs(_ context.Context, nids []types.StateSnapshotNID) ([]types.StateBlockNIDList, error) {
	lists := make([]types.StateBlockNIDList, 0, len(nids))
	for _, nid := range nids {
		lists = append(lists, types.StateBlockNIDList{StateSnapshotNID: nid, StateBlockNIDs: slices.Clone(m.snapshots[nid])})
	}
	return lists, nil
}

func (m *memStateStorage) StateEntries(_ context.Context, nids []types.StateBlockNID) ([]types.StateEntryList, error) {
	lists := make([]types.StateEntryList, 0, len(nids))
	for _, nid := range nids {
		lists = append(lists, types.StateEntryList{StateBlockNID: nid, StateEntries: slices.Clone(m.blocks[nid])})
	}
	return lists, nil
}

func (m *memStateStorage) store(blocks []types.StateBlockNID, state []types.StateEntry) types.StateSnapshotNID {
	blocks = slices.Clone(blocks)
	if len(state) > 0 {
		blockNID := types.StateBlockNID(len(m.blocks) + 1)
		entries := slices.Clone(state)
		sort.Sort(stateEntrySorter(entries))
		m.blocks[blockNID] = entries
		blocks = append(blocks, blockNID)
	}
	snapshotNID := types.StateSnapshotNID(len(m.snapshots) + 1)
	m.snapshots[snapshotNID] = blocks
	return snapshotNID
}

func (m *memStateStorage) AddState(_ context.Context, _ types.RoomNID, blocks []types.StateBlockNID, state []types.StateEntry) (types.StateSnapshotNID, error) {
	m.fullAdds++
	return m.store(blocks, state), nil
}

func (m *memStateStorage) AddStateDelta(_ context.Context, _ types.RoomNID, blocks []types.StateBlockNID, delta []types.StateEntry) (types.StateSnapshotNID, error) {
	m.deltaAdds++
	return m.store(blocks, delta), nil
}

// load combines a snapshot's blocks the way LoadStateAtSnapshot does.
func (m *memStateStorage) load(t *testing.T, nid types.StateSnapshotNID) map[types.StateKeyTuple]types.EventNID {
	t.Helper()
	v := StateResolution{db: m, roomInfo: &types.RoomInfo{}}
	entries, err := v.LoadStateAtSnapshot(context.Background(), nid)
	if err != nil {
		t.Fatalf("LoadStateAtSnapshot: %v", err)
	}
	return stateMap(entries)
}

func (m *memStateStorage) blockEntries() int {
	n := 0
	for _, entries := range m.blocks {
		n += len(entries)
	}
	return n
}

func stateMap(entries []types.StateEntry) map[types.StateKeyTuple]types.EventNID {
	out := make(map[types.StateKeyTuple]types.EventNID, len(entries))
	for _, e := range entries {
		out[e.StateKeyTuple] = e.EventNID
	}
	return out
}

func entry(key types.EventStateKeyNID, event types.EventNID) types.StateEntry {
	return types.StateEntry{StateKeyTuple: types.StateKeyTuple{EventTypeNID: types.MRoomMemberNID, EventStateKeyNID: key}, EventNID: event}
}

func roomState(members int, firstEvent types.EventNID) []types.StateEntry {
	state := make([]types.StateEntry, 0, members)
	for i := range members {
		state = append(state, entry(types.EventStateKeyNID(i+1), firstEvent+types.EventNID(i)))
	}
	return state
}

func withEntries(state []types.StateEntry, changes ...types.StateEntry) []types.StateEntry {
	byKey := stateMap(state)
	for _, c := range changes {
		byKey[c.StateKeyTuple] = c.EventNID
	}
	out := make([]types.StateEntry, 0, len(byKey))
	for k, e := range byKey {
		out = append(out, types.StateEntry{StateKeyTuple: k, EventNID: e})
	}
	sort.Sort(stateEntrySorter(out))
	return out
}

func requireState(t *testing.T, m *memStateStorage, nid types.StateSnapshotNID, want []types.StateEntry) {
	t.Helper()
	got := m.load(t, nid)
	if len(got) != len(want) {
		t.Fatalf("snapshot %d has %d entries, want %d", nid, len(got), len(want))
	}
	for k, e := range stateMap(want) {
		if got[k] != e {
			t.Fatalf("snapshot %d has event %d for %v, want %d", nid, got[k], k, e)
		}
	}
}

func TestStoreResolvedStateMergeIsDelta(t *testing.T) {
	ctx := context.Background()
	m := newMemStateStorage()
	v := StateResolution{db: m, roomInfo: &types.RoomInfo{}}

	base := roomState(1000, 1)
	baseNID := m.store(nil, base)
	branchA := withEntries(base, entry(5000, 9001))
	branchANID := m.store(m.snapshots[baseNID], []types.StateEntry{entry(5000, 9001)})
	merged := withEntries(branchA, entry(5001, 9002))

	nid, asDelta, err := v.storeResolvedState(ctx, 1, []types.StateAtEvent{
		{BeforeStateSnapshotNID: baseNID}, {BeforeStateSnapshotNID: branchANID},
	}, merged)
	if err != nil {
		t.Fatal(err)
	}
	if !asDelta {
		t.Fatal("a merge that adds one member was stored as a full block")
	}
	requireState(t, m, nid, merged)
	if got := len(m.blocks[m.snapshots[nid][len(m.snapshots[nid])-1]]); got != 1 {
		t.Fatalf("delta block has %d entries, want 1", got)
	}
}

func TestStoreResolvedStateRestoresOverriddenEvent(t *testing.T) {
	ctx := context.Background()
	m := newMemStateStorage()
	v := StateResolution{db: m, roomInfo: &types.RoomInfo{}}

	// The base block holds event 1 for key 1 and a later block replaces it
	// with event 2000; resolution then picks event 1 again.
	base := roomState(100, 1)
	baseNID := m.store(nil, base)
	overriddenNID := m.store(m.snapshots[baseNID], []types.StateEntry{entry(1, 2000)})
	resolved := withEntries(base, entry(200, 3000))

	nid, asDelta, err := v.storeResolvedState(ctx, 1, []types.StateAtEvent{{BeforeStateSnapshotNID: overriddenNID}}, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !asDelta {
		t.Fatal("expected a delta")
	}
	requireState(t, m, nid, resolved)
}

func TestStoreResolvedStateMissingKeyIsFull(t *testing.T) {
	ctx := context.Background()
	m := newMemStateStorage()
	v := StateResolution{db: m, roomInfo: &types.RoomInfo{}}

	base := roomState(100, 1)
	baseNID := m.store(nil, base)
	// A resolved state without key 1 cannot extend a base that has it.
	resolved := slices.Clone(base[1:])

	nid, asDelta, err := v.storeResolvedState(ctx, 1, []types.StateAtEvent{{BeforeStateSnapshotNID: baseNID}}, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if asDelta {
		t.Fatal("a state without one of the base keys was stored as a delta")
	}
	requireState(t, m, nid, resolved)
}

func TestStoreResolvedStateLargeChangeIsFull(t *testing.T) {
	ctx := context.Background()
	m := newMemStateStorage()
	v := StateResolution{db: m, roomInfo: &types.RoomInfo{}}

	base := roomState(100, 1)
	baseNID := m.store(nil, base)
	resolved := withEntries(base, roomState(100, 5000)[:50]...)

	nid, asDelta, err := v.storeResolvedState(ctx, 1, []types.StateAtEvent{{BeforeStateSnapshotNID: baseNID}}, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if asDelta {
		t.Fatal("a state that changes half the entries was stored as a delta")
	}
	requireState(t, m, nid, resolved)
}

func TestStoreResolvedStateFoldsLongChain(t *testing.T) {
	ctx := context.Background()
	m := newMemStateStorage()
	v := StateResolution{db: m, roomInfo: &types.RoomInfo{}}

	state := roomState(2000, 1)
	nid := m.store(nil, state)
	// Grow a chain of single-entry blocks to the limit, as single_delta does.
	for i := range maxStateBlockNIDs - 1 {
		change := entry(types.EventStateKeyNID(10000+i), types.EventNID(50000+i))
		state = withEntries(state, change)
		nid = m.store(m.snapshots[nid], []types.StateEntry{change})
	}
	if len(m.snapshots[nid]) != maxStateBlockNIDs {
		t.Fatalf("chain has %d blocks, want %d", len(m.snapshots[nid]), maxStateBlockNIDs)
	}
	before := m.blockEntries()
	state = withEntries(state, entry(20000, 60000))

	next, asDelta, err := v.storeResolvedState(ctx, 1, []types.StateAtEvent{{BeforeStateSnapshotNID: nid}}, state)
	if err != nil {
		t.Fatal(err)
	}
	if !asDelta {
		t.Fatal("a full chain was rewritten as a full block")
	}
	requireState(t, m, next, state)
	if got := len(m.snapshots[next]); got != 2 {
		t.Fatalf("folded snapshot has %d blocks, want the base and one delta", got)
	}
	if added := m.blockEntries() - before; added != maxStateBlockNIDs {
		t.Fatalf("folding stored %d entries, want %d", added, maxStateBlockNIDs)
	}
}

func TestStateDelta(t *testing.T) {
	base := stateMap(roomState(3, 1))
	delta, ok := stateDelta(base, withEntries(roomState(3, 1), entry(2, 99), entry(4, 100)))
	if !ok {
		t.Fatal("expected a delta")
	}
	want := []types.StateEntry{entry(2, 99), entry(4, 100)}
	sort.Sort(stateEntrySorter(delta))
	if !slices.Equal(delta, want) {
		t.Fatalf("delta %v, want %v", delta, want)
	}
	if _, ok := stateDelta(base, roomState(2, 1)); ok {
		t.Fatal("a state without one of the base keys gave a delta")
	}
	if _, ok := stateDelta(base, append(roomState(3, 1), entry(1, 7))); ok {
		t.Fatal("a state with two entries for one key gave a delta")
	}
}
