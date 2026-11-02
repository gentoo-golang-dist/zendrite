// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package api

import "context"

// ThreadSubscriptionsAPI stores MSC4306 thread subscriptions. Every change
// gets a position in the thread subscriptions stream, so that sliding sync
// (MSC4308) can report it.
type ThreadSubscriptionsAPI interface {
	// PerformThreadSubscribe subscribes a user to a thread.
	PerformThreadSubscribe(ctx context.Context, req *PerformThreadSubscribeRequest) (*PerformThreadSubscribeResponse, error)
	// PerformThreadUnsubscribe unsubscribes a user from a thread. It returns
	// the stream position of the change, or 0 when the user was not
	// subscribed.
	PerformThreadUnsubscribe(ctx context.Context, req *PerformThreadUnsubscribeRequest) (int64, error)
	// QueryThreadSubscription returns a user's subscription to a thread, or
	// nil when the user is not subscribed.
	QueryThreadSubscription(ctx context.Context, userID, roomID, threadRootID string) (*ThreadSubscription, error)
	// QueryThreadSubscriptionUpdates returns the latest changes to a user's
	// thread subscriptions with stream positions in (from, to], at most
	// limit of them, in ascending stream order. When there are more, the
	// oldest are left out.
	QueryThreadSubscriptionUpdates(ctx context.Context, userID string, from, to int64, limit int) ([]ThreadSubscriptionUpdate, error)
	// QueryMaxThreadSubscriptionStreamID returns the latest position in the
	// thread subscriptions stream.
	QueryMaxThreadSubscriptionStreamID(ctx context.Context) (int64, error)
}

// ThreadSubscription is a user's subscription to a thread.
type ThreadSubscription struct {
	// Automatic is true when the user's client subscribed automatically,
	// for example because the user was mentioned in the thread.
	Automatic bool
}

// ThreadSubscriptionUpdate is the latest state of a user's subscription to
// a thread, as seen in the thread subscriptions stream.
type ThreadSubscriptionUpdate struct {
	StreamID     int64
	RoomID       string
	ThreadRootID string
	Subscribed   bool
	// Automatic is only meaningful when Subscribed is true.
	Automatic bool
}

// PerformThreadSubscribeRequest subscribes a user to a thread.
type PerformThreadSubscribeRequest struct {
	UserID       string
	RoomID       string
	ThreadRootID string
	// Automatic is true for automatic subscriptions, whose cause event is
	// at CauseStreamPos.
	Automatic bool
	// CauseStreamPos is the sync stream position of the event that caused
	// an automatic subscription. An automatic subscription is skipped when
	// the user unsubscribed after that event arrived.
	CauseStreamPos int64
}

// PerformThreadSubscribeResponse reports the outcome of a subscription.
type PerformThreadSubscribeResponse struct {
	// StreamID is the stream position of the change, or 0 when the existing
	// subscription already covers the request.
	StreamID int64
	// Conflict is true when an automatic subscription was skipped because
	// the user unsubscribed after its cause event arrived.
	Conflict bool
}

// PerformThreadUnsubscribeRequest unsubscribes a user from a thread.
type PerformThreadUnsubscribeRequest struct {
	UserID       string
	RoomID       string
	ThreadRootID string
	// RoomStreamPos is the sync stream position of the latest event in the
	// room. Automatic subscriptions caused by events up to it are skipped.
	RoomStreamPos int64
}
