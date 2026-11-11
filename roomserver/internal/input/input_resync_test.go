// Copyright 2025 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.
package input

import (
	"fmt"
	"testing"

	"codefloe.com/pat-s/zendrite/roomserver/types"
)

func memberEntry(stateKey types.EventStateKeyNID, eventNID types.EventNID) types.StateEntry {
	return types.StateEntry{
		StateKeyTuple: types.StateKeyTuple{EventTypeNID: types.MRoomMemberNID, EventStateKeyNID: stateKey},
		EventNID:      eventNID,
	}
}

func nonMemberEntry(typeNID types.EventTypeNID, eventNID types.EventNID) types.StateEntry {
	return types.StateEntry{
		StateKeyTuple: types.StateKeyTuple{EventTypeNID: typeNID, EventStateKeyNID: 1},
		EventNID:      eventNID,
	}
}

func memberNIDForKey(entries []types.StateEntry, stateKey types.EventStateKeyNID) types.EventNID {
	for _, e := range entries {
		if e.EventTypeNID == types.MRoomMemberNID && e.EventStateKeyNID == stateKey {
			return e.EventNID
		}
	}
	return 0
}

const (
	localKey  types.EventStateKeyNID = 65536
	remoteKey types.EventStateKeyNID = 170655
)

func TestResyncStateIncomplete(t *testing.T) {
	cases := []struct {
		name           string
		requestedIDs   []string
		loaded         int
		wantDistinct   int
		wantIncomplete bool
	}{
		{
			name:           "all requested events loaded",
			requestedIDs:   []string{"$a", "$b", "$c"},
			loaded:         3,
			wantDistinct:   3,
			wantIncomplete: false,
		},
		{
			name:           "shortfall is incomplete",
			requestedIDs:   []string{"$a", "$b", "$c"},
			loaded:         2,
			wantDistinct:   3,
			wantIncomplete: true,
		},
		{
			name:           "catastrophic shortfall (issue #247: 35 of ~1.2k)",
			requestedIDs:   makeIDs(1200),
			loaded:         35,
			wantDistinct:   1200,
			wantIncomplete: true,
		},
		{
			name:           "zero loaded with requested is incomplete",
			requestedIDs:   []string{"$a"},
			loaded:         0,
			wantDistinct:   1,
			wantIncomplete: true,
		},
		{
			name: "duplicate requested IDs are deduplicated, not a shortfall",
			// /state_ids repeats an ID; loaded matches the distinct count.
			requestedIDs:   []string{"$a", "$b", "$a"},
			loaded:         2,
			wantDistinct:   2,
			wantIncomplete: false,
		},
		{
			name:           "nothing requested is a genuine no-op",
			requestedIDs:   nil,
			loaded:         0,
			wantDistinct:   0,
			wantIncomplete: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			distinct, incomplete := resyncStateIncomplete(tc.requestedIDs, tc.loaded)
			if distinct != tc.wantDistinct {
				t.Fatalf("distinct = %d, want %d", distinct, tc.wantDistinct)
			}
			if incomplete != tc.wantIncomplete {
				t.Fatalf("incomplete = %v, want %v", incomplete, tc.wantIncomplete)
			}
		})
	}
}

func makeIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("$event%d", i)
	}
	return ids
}

// Only keys the extremity lacks are filled in. A key it already has keeps its
// own event, since that can be newer than the resync state.
func TestMissingStateEntries(t *testing.T) {
	have := []types.StateEntry{
		nonMemberEntry(types.MRoomCreateNID, 1),
		memberEntry(localKey, 10), // newer than the resync's entry for this key
	}
	resync := []types.StateEntry{
		nonMemberEntry(types.MRoomCreateNID, 1),
		memberEntry(localKey, 5),
		memberEntry(remoteKey, 20),
	}
	missing := missingStateEntries(have, resync)
	if len(missing) != 1 || missing[0] != memberEntry(remoteKey, 20) {
		t.Fatalf("expected only the remote member to be filled in, got %v", missing)
	}
	if got := missingStateEntries(resync, resync); len(got) != 0 {
		t.Fatalf("expected nothing missing from complete state, got %v", got)
	}
}
