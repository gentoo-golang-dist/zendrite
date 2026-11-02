// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package storage_test

import (
	"sync"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

func TestThreadSubscriptions(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := t.Context()
		db, closeDB := mustCreateUserDatabase(t, dbType)
		defer closeDB()
		const room, root = "!room:localhost", "$root"
		var alice, bob spec.ServerName = "localhost", "localhost"

		maxID, err := db.MaxThreadSubscriptionStreamID(ctx)
		require.NoError(t, err)
		require.Zero(t, maxID)
		sub, err := db.GetThreadSubscription(ctx, "alice", alice, room, root)
		require.NoError(t, err)
		require.Nil(t, sub)

		// An automatic subscription.
		id1, conflict, err := db.SubscribeThread(ctx, "alice", alice, room, root, true, 10)
		require.NoError(t, err)
		require.False(t, conflict)
		require.Equal(t, int64(1), id1)
		sub, err = db.GetThreadSubscription(ctx, "alice", alice, room, root)
		require.NoError(t, err)
		require.Equal(t, &api.ThreadSubscription{Automatic: true}, sub)

		// Another automatic subscription changes nothing.
		id, _, err := db.SubscribeThread(ctx, "alice", alice, room, root, true, 11)
		require.NoError(t, err)
		require.Zero(t, id)

		// A manual subscription takes precedence over the automatic one.
		id2, _, err := db.SubscribeThread(ctx, "alice", alice, room, root, false, 0)
		require.NoError(t, err)
		require.Equal(t, int64(2), id2)
		sub, err = db.GetThreadSubscription(ctx, "alice", alice, room, root)
		require.NoError(t, err)
		require.Equal(t, &api.ThreadSubscription{Automatic: false}, sub)

		// Neither kind of subscription downgrades a manual one.
		for _, automatic := range []bool{true, false} {
			id, _, err = db.SubscribeThread(ctx, "alice", alice, room, root, automatic, 12)
			require.NoError(t, err)
			require.Zero(t, id)
		}

		// Bob's subscription is separate.
		id3, _, err := db.SubscribeThread(ctx, "bob", bob, room, root, false, 0)
		require.NoError(t, err)
		require.Equal(t, int64(3), id3)
		subscribers, err := db.GetThreadSubscribers(ctx, room, root)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"@alice:localhost", "@bob:localhost"}, subscribers)

		// Alice unsubscribes when the room's latest event is at 20.
		id4, err := db.UnsubscribeThread(ctx, "alice", alice, room, root, 20)
		require.NoError(t, err)
		require.Equal(t, int64(4), id4)
		sub, err = db.GetThreadSubscription(ctx, "alice", alice, room, root)
		require.NoError(t, err)
		require.Nil(t, sub)
		subscribers, err = db.GetThreadSubscribers(ctx, room, root)
		require.NoError(t, err)
		require.Equal(t, []string{"@bob:localhost"}, subscribers)

		// Unsubscribing again, or from a thread never subscribed, is a no-op.
		id, err = db.UnsubscribeThread(ctx, "alice", alice, room, root, 25)
		require.NoError(t, err)
		require.Zero(t, id)
		id, err = db.UnsubscribeThread(ctx, "alice", alice, room, "$other", 25)
		require.NoError(t, err)
		require.Zero(t, id)

		// Automatic subscriptions caused by events the unsubscription had
		// already seen are skipped; later ones apply.
		for _, cause := range []int64{5, 20} {
			id, conflict, err = db.SubscribeThread(ctx, "alice", alice, room, root, true, cause)
			require.NoError(t, err)
			require.True(t, conflict, "cause %d", cause)
			require.Zero(t, id)
		}
		id5, conflict, err := db.SubscribeThread(ctx, "alice", alice, room, root, true, 21)
		require.NoError(t, err)
		require.False(t, conflict)
		require.Equal(t, int64(5), id5)

		// A manual subscription always applies after an unsubscription.
		_, err = db.UnsubscribeThread(ctx, "alice", alice, room, root, 30)
		require.NoError(t, err)
		id7, conflict, err := db.SubscribeThread(ctx, "alice", alice, room, root, false, 0)
		require.NoError(t, err)
		require.False(t, conflict)
		require.Equal(t, int64(7), id7)

		// Updates report the latest state per thread, oldest first.
		_, err = db.UnsubscribeThread(ctx, "alice", alice, room, root, 30)
		require.NoError(t, err)
		id9, _, err := db.SubscribeThread(ctx, "alice", alice, room, "$root2", true, 1)
		require.NoError(t, err)
		require.Equal(t, int64(9), id9)
		updates, err := db.GetThreadSubscriptionUpdates(ctx, "alice", alice, 0, 9, 10)
		require.NoError(t, err)
		require.Equal(t, []api.ThreadSubscriptionUpdate{
			{StreamID: 8, RoomID: room, ThreadRootID: root, Subscribed: false},
			{StreamID: 9, RoomID: room, ThreadRootID: "$root2", Subscribed: true, Automatic: true},
		}, updates)
		// The limit keeps the latest changes; the bounds are (from, to].
		updates, err = db.GetThreadSubscriptionUpdates(ctx, "alice", alice, 0, 9, 1)
		require.NoError(t, err)
		require.Len(t, updates, 1)
		require.Equal(t, int64(9), updates[0].StreamID)
		updates, err = db.GetThreadSubscriptionUpdates(ctx, "alice", alice, 8, 9, 10)
		require.NoError(t, err)
		require.Len(t, updates, 1)
		updates, err = db.GetThreadSubscriptionUpdates(ctx, "alice", alice, 0, 8, 10)
		require.NoError(t, err)
		require.Len(t, updates, 1)
		require.Equal(t, int64(8), updates[0].StreamID)

		maxID, err = db.MaxThreadSubscriptionStreamID(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(9), maxID)
	})
}

// Concurrent subscriptions, as from several devices of one user, get distinct
// stream positions and leave one consistent row per thread.
func TestThreadSubscriptionsConcurrent(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := t.Context()
		db, closeDB := mustCreateUserDatabase(t, dbType)
		defer closeDB()
		const writers = 8
		var wg sync.WaitGroup
		ids := make(chan int64, writers*2)
		errs := make(chan error, writers*2)
		for i := range writers {
			wg.Go(func() {
				id, _, err := db.SubscribeThread(ctx, "alice", "localhost", "!room:localhost", "$root", i%2 == 0, int64(i+1))
				errs <- err
				ids <- id
				id, _, err = db.SubscribeThread(ctx, "bob", "localhost", "!room:localhost", "$root", false, 0)
				errs <- err
				ids <- id
			})
		}
		wg.Wait()
		close(ids)
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		seen := map[int64]bool{}
		for id := range ids {
			if id == 0 {
				continue
			}
			require.False(t, seen[id], "stream position %d handed out twice", id)
			seen[id] = true
		}
		maxID, err := db.MaxThreadSubscriptionStreamID(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(len(seen)), maxID)
		sub, err := db.GetThreadSubscription(ctx, "alice", "localhost", "!room:localhost", "$root")
		require.NoError(t, err)
		require.NotNil(t, sub)
		subscribers, err := db.GetThreadSubscribers(ctx, "!room:localhost", "$root")
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"@alice:localhost", "@bob:localhost"}, subscribers)
	})
}
