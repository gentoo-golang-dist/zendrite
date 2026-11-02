// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared

import (
	"context"
	"database/sql"
	"errors"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/userapi/api"
	"codefloe.com/pat-s/zendrite/userapi/storage/tables"
)

// SubscribeThread subscribes a user to a thread (MSC4306) and returns the
// stream position of the change, or 0 when the existing subscription already
// covers the request: a manual subscription is never downgraded to an
// automatic one. An automatic subscription whose cause event is at or before
// the room position recorded at the user's latest unsubscription is skipped
// and reported as a conflict, so that a client which has not seen the
// unsubscription cannot undo it.
func (d *Database) SubscribeThread(
	ctx context.Context, localpart string, serverName spec.ServerName, roomID, threadRootID string,
	automatic bool, causeStreamPos int64,
) (streamID int64, conflict bool, err error) {
	err = d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		if err := d.ThreadSubscriptions.LockForWrite(ctx, txn); err != nil {
			return err
		}
		row, err := d.ThreadSubscriptions.SelectThreadSubscription(ctx, txn, localpart, serverName, roomID, threadRootID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if row != nil {
			if row.Subscribed && (!row.Automatic || automatic) {
				return nil
			}
			if !row.Subscribed && automatic && causeStreamPos <= row.UnsubscribedAt {
				conflict = true
				return nil
			}
		}
		if streamID, err = d.nextThreadSubscriptionStreamID(ctx, txn); err != nil {
			return err
		}
		return d.ThreadSubscriptions.UpsertThreadSubscription(ctx, txn, localpart, serverName, roomID, threadRootID, &tables.ThreadSubscriptionRow{
			Subscribed: true,
			Automatic:  automatic,
			StreamID:   streamID,
		})
	})
	if err != nil || conflict {
		return 0, conflict, err
	}
	return streamID, false, nil
}

// UnsubscribeThread unsubscribes a user from a thread (MSC4306) and returns
// the stream position of the change, or 0 when the user was not subscribed.
// The position of the room's latest event, roomStreamPos, orders later
// automatic subscriptions against this unsubscription.
func (d *Database) UnsubscribeThread(
	ctx context.Context, localpart string, serverName spec.ServerName, roomID, threadRootID string, roomStreamPos int64,
) (streamID int64, err error) {
	err = d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		if err := d.ThreadSubscriptions.LockForWrite(ctx, txn); err != nil {
			return err
		}
		row, err := d.ThreadSubscriptions.SelectThreadSubscription(ctx, txn, localpart, serverName, roomID, threadRootID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil || !row.Subscribed {
			return err
		}
		if streamID, err = d.nextThreadSubscriptionStreamID(ctx, txn); err != nil {
			return err
		}
		return d.ThreadSubscriptions.UpsertThreadSubscription(ctx, txn, localpart, serverName, roomID, threadRootID, &tables.ThreadSubscriptionRow{
			Subscribed:     false,
			StreamID:       streamID,
			UnsubscribedAt: roomStreamPos,
		})
	})
	return streamID, err
}

// nextThreadSubscriptionStreamID returns the next stream position. Rows are
// never deleted and only move forwards, so the maximum never decreases; the
// caller holds the write lock, so positions are committed in order and a
// reader never sees a position before an earlier one.
func (d *Database) nextThreadSubscriptionStreamID(ctx context.Context, txn *sql.Tx) (int64, error) {
	maxID, err := d.ThreadSubscriptions.SelectMaxStreamID(ctx, txn)
	return maxID + 1, err
}

// GetThreadSubscription returns a user's subscription to a thread, or nil
// when the user is not subscribed.
func (d *Database) GetThreadSubscription(
	ctx context.Context, localpart string, serverName spec.ServerName, roomID, threadRootID string,
) (*api.ThreadSubscription, error) {
	row, err := d.ThreadSubscriptions.SelectThreadSubscription(ctx, nil, localpart, serverName, roomID, threadRootID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || !row.Subscribed {
		return nil, err
	}
	return &api.ThreadSubscription{Automatic: row.Automatic}, nil
}

// GetThreadSubscriptionUpdates returns the latest changes to a user's thread
// subscriptions in (from, to], see api.ThreadSubscriptionsAPI.
func (d *Database) GetThreadSubscriptionUpdates(
	ctx context.Context, localpart string, serverName spec.ServerName, from, to int64, limit int,
) ([]api.ThreadSubscriptionUpdate, error) {
	if limit <= 0 || to <= from {
		return nil, nil
	}
	return d.ThreadSubscriptions.SelectThreadSubscriptionUpdates(ctx, nil, localpart, serverName, from, to, limit)
}

// GetThreadSubscribers returns the local users subscribed to a thread.
func (d *Database) GetThreadSubscribers(ctx context.Context, roomID, threadRootID string) ([]string, error) {
	return d.ThreadSubscriptions.SelectThreadSubscribers(ctx, nil, roomID, threadRootID)
}

// MaxThreadSubscriptionStreamID returns the latest position in the thread
// subscriptions stream.
func (d *Database) MaxThreadSubscriptionStreamID(ctx context.Context) (int64, error) {
	return d.ThreadSubscriptions.SelectMaxStreamID(ctx, nil)
}
