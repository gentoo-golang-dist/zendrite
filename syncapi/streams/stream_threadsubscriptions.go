// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package streams

import (
	"context"

	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// ThreadSubscriptionsStreamProvider tracks the latest position of the MSC4306
// thread subscriptions stream, which the userapi stores. Only the sliding sync
// extension (MSC4308) returns its changes; /sync merely carries its position
// in the token.
type ThreadSubscriptionsStreamProvider struct {
	DefaultStreamProvider
	userAPI userapi.SyncUserAPI
}

func (p *ThreadSubscriptionsStreamProvider) Setup(
	ctx context.Context, snapshot storage.DatabaseTransaction,
) {
	p.DefaultStreamProvider.Setup(ctx, snapshot)

	p.latestMutex.Lock()
	defer p.latestMutex.Unlock()

	id, err := p.userAPI.QueryMaxThreadSubscriptionStreamID(ctx)
	if err != nil {
		panic(err)
	}
	p.latest = types.StreamPosition(id)
}

func (p *ThreadSubscriptionsStreamProvider) CompleteSync(
	ctx context.Context,
	_ storage.DatabaseTransaction,
	_ *types.SyncRequest,
) types.StreamPosition {
	return p.LatestPosition(ctx)
}

func (p *ThreadSubscriptionsStreamProvider) IncrementalSync(
	_ context.Context,
	_ storage.DatabaseTransaction,
	_ *types.SyncRequest,
	_, to types.StreamPosition,
) types.StreamPosition {
	return to
}
