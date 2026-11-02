// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package streams

import (
	"context"

	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

// ProfileUpdatesStreamProvider tracks the latest position in the profile
// updates stream. Only the MSC4262 sliding sync extension delivers profile
// updates; /sync just carries the position forward, so that its tokens catch
// up with the stream and a long poll does not wake for updates it never
// returns.
type ProfileUpdatesStreamProvider struct {
	DefaultStreamProvider
}

func (p *ProfileUpdatesStreamProvider) Setup(
	ctx context.Context, snapshot storage.DatabaseTransaction,
) {
	p.DefaultStreamProvider.Setup(ctx, snapshot)

	p.latestMutex.Lock()
	defer p.latestMutex.Unlock()

	id, err := snapshot.MaxStreamPositionForProfileUpdates(ctx)
	if err != nil {
		panic(err)
	}
	p.latest = id
}

func (p *ProfileUpdatesStreamProvider) CompleteSync(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	req *types.SyncRequest,
) types.StreamPosition {
	return p.LatestPosition(ctx)
}

func (p *ProfileUpdatesStreamProvider) IncrementalSync(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	req *types.SyncRequest,
	from, to types.StreamPosition,
) types.StreamPosition {
	return max(from, to)
}
