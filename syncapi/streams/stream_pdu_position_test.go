// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package streams

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/syncapi/notifier"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

// aheadSnapshot holds an event at pos.
type aheadSnapshot struct {
	storage.DatabaseTransaction
	pos types.StreamPosition
}

func (s aheadSnapshot) LatestSyncEventAfter(_ context.Context, after types.StreamPosition) (types.StreamPosition, error) {
	if s.pos > after {
		return s.pos, nil
	}
	return 0, nil
}

// An event is visible to a snapshot as soon as its transaction commits, before
// the sync position advances past it. A complete sync returns it in the
// current state, so a token from before it would return it again in the next
// timeline.
func TestCompleteSyncPositionWaitsForTheSnapshot(t *testing.T) {
	const latest, ahead = types.StreamPosition(10), types.StreamPosition(11)
	req := &types.SyncRequest{Log: logrus.NewEntry(logrus.StandardLogger())}
	newProvider := func() *PDUStreamProvider {
		return &PDUStreamProvider{notifier: notifier.NewNotifier(nil)}
	}

	t.Run("the notifier catches up", func(t *testing.T) {
		provider := newProvider()
		go func() {
			time.Sleep(20 * time.Millisecond)
			provider.notifier.OnNewEvent(nil, "", nil, types.StreamingToken{PDUPosition: ahead})
		}()
		require.Equal(t, ahead, provider.positionOfSnapshot(t.Context(), aheadSnapshot{pos: ahead}, req, latest))
	})

	t.Run("nothing newer does not wait", func(t *testing.T) {
		start := time.Now()
		require.Equal(t, latest, newProvider().positionOfSnapshot(t.Context(), aheadSnapshot{pos: latest}, req, latest))
		require.Less(t, time.Since(start), snapshotPositionTimeout/2)
	})

	// A token must never pass the notifier, or the next sync runs backwards.
	t.Run("the notifier does not catch up", func(t *testing.T) {
		require.Equal(t, latest, newProvider().positionOfSnapshot(t.Context(), aheadSnapshot{pos: ahead}, req, latest))
	})

	t.Run("the client goes away", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(nil)
		start := time.Now()
		require.Equal(t, latest, newProvider().positionOfSnapshot(ctx, aheadSnapshot{pos: ahead}, req, latest))
		require.Less(t, time.Since(start), snapshotPositionTimeout/2)
	})
}
