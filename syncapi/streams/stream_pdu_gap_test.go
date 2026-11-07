// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package streams

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

// gapSnapshot reports the given events as backward extremities of the room.
type gapSnapshot struct {
	storage.DatabaseTransaction
	extremities map[string][]string
}

func (s gapSnapshot) BackwardExtremitiesForRoom(context.Context, string) (map[string][]string, error) {
	return s.extremities, nil
}

func TestAfterGap(t *testing.T) {
	before := eventAtDepth(t, "before", 1)
	gap := eventAtDepth(t, "gap", 10)
	after := eventAtDepth(t, "after", 11)
	recent := types.RecentEvents{Events: []types.StreamEvent{before, gap, after}}
	ids := func(recent types.RecentEvents) []string {
		var ids []string
		for _, ev := range recent.Events {
			ids = append(ids, ev.EventID())
		}
		return ids
	}

	t.Run("no gap", func(t *testing.T) {
		got, err := afterGap(t.Context(), gapSnapshot{}, "!a:test", recent)
		require.NoError(t, err)
		require.Equal(t, []string{"$before", "$gap", "$after"}, ids(got))
		require.False(t, got.Limited)
	})

	t.Run("the timeline starts at the gap", func(t *testing.T) {
		snapshot := gapSnapshot{extremities: map[string][]string{"$gap": {"$missing"}}}
		got, err := afterGap(t.Context(), snapshot, "!a:test", recent)
		require.NoError(t, err)
		require.Equal(t, []string{"$gap", "$after"}, ids(got))
		require.True(t, got.Limited)
	})

	// The events before the gap came in an earlier sync. The gap is still
	// between them and the timeline.
	t.Run("a gap at the start", func(t *testing.T) {
		snapshot := gapSnapshot{extremities: map[string][]string{"$before": {"$missing"}}}
		got, err := afterGap(t.Context(), snapshot, "!a:test", recent)
		require.NoError(t, err)
		require.Equal(t, []string{"$before", "$gap", "$after"}, ids(got))
		require.True(t, got.Limited)
	})

	t.Run("an extremity outside the timeline", func(t *testing.T) {
		snapshot := gapSnapshot{extremities: map[string][]string{"$older": {"$missing"}}}
		got, err := afterGap(t.Context(), snapshot, "!a:test", recent)
		require.NoError(t, err)
		require.Equal(t, []string{"$before", "$gap", "$after"}, ids(got))
		require.False(t, got.Limited)
	})
}
