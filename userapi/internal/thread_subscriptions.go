// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"
	"fmt"

	"codefloe.com/pat-s/gomatrixserverlib"

	"codefloe.com/pat-s/zendrite/userapi/api"
)

func (a *UserInternalAPI) PerformThreadSubscribe(ctx context.Context, req *api.PerformThreadSubscribeRequest) (*api.PerformThreadSubscribeResponse, error) {
	localpart, domain, err := gomatrixserverlib.SplitID('@', req.UserID)
	if err != nil {
		return nil, fmt.Errorf("failed to split user ID %q: %w", req.UserID, err)
	}
	streamID, conflict, err := a.DB.SubscribeThread(ctx, localpart, domain, req.RoomID, req.ThreadRootID, req.Automatic, req.CauseStreamPos)
	if err != nil {
		return nil, err
	}
	return &api.PerformThreadSubscribeResponse{StreamID: streamID, Conflict: conflict}, nil
}

func (a *UserInternalAPI) PerformThreadUnsubscribe(ctx context.Context, req *api.PerformThreadUnsubscribeRequest) (int64, error) {
	localpart, domain, err := gomatrixserverlib.SplitID('@', req.UserID)
	if err != nil {
		return 0, fmt.Errorf("failed to split user ID %q: %w", req.UserID, err)
	}
	return a.DB.UnsubscribeThread(ctx, localpart, domain, req.RoomID, req.ThreadRootID, req.RoomStreamPos)
}

func (a *UserInternalAPI) QueryThreadSubscription(ctx context.Context, userID, roomID, threadRootID string) (*api.ThreadSubscription, error) {
	localpart, domain, err := gomatrixserverlib.SplitID('@', userID)
	if err != nil {
		return nil, fmt.Errorf("failed to split user ID %q: %w", userID, err)
	}
	return a.DB.GetThreadSubscription(ctx, localpart, domain, roomID, threadRootID)
}

func (a *UserInternalAPI) QueryThreadSubscriptionUpdates(ctx context.Context, userID string, from, to int64, limit int) ([]api.ThreadSubscriptionUpdate, error) {
	localpart, domain, err := gomatrixserverlib.SplitID('@', userID)
	if err != nil {
		return nil, fmt.Errorf("failed to split user ID %q: %w", userID, err)
	}
	return a.DB.GetThreadSubscriptionUpdates(ctx, localpart, domain, from, to, limit)
}

func (a *UserInternalAPI) QueryMaxThreadSubscriptionStreamID(ctx context.Context) (int64, error) {
	return a.DB.MaxThreadSubscriptionStreamID(ctx)
}
