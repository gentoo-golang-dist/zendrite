// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package storage_test

import (
	"fmt"
	"sync"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

func TestRendezvousSessions(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := mustCreateUserDatabase(t, dbType)
		defer closeDB()

		const now = spec.Timestamp(1_000_000)
		newSession := func(id string, kind api.RendezvousKind, expires spec.Timestamp) {
			t.Helper()
			require.NoError(t, db.CreateRendezvousSession(ctx, &api.RendezvousSession{
				ID: id, Kind: kind, Data: []byte("hello"), ContentType: "text/plain",
				Sequence: 1, LastModified: now, Expires: expires, Anonymous: true,
			}, 0, 0))
		}
		write := func(id, txnID string, expected int64, data string, at spec.Timestamp) (*api.RendezvousSession, error) {
			return db.WriteRendezvousSession(ctx, &api.RendezvousWrite{
				ID: id, Kind: api.RendezvousKindMSC4388, ExpectedSequence: expected,
				Data: []byte(data), TxnID: txnID, MaxTxns: 3, Now: at,
			})
		}

		t.Run("compare and swap", func(t *testing.T) {
			newSession("cas", api.RendezvousKindMSC4388, now+1000)
			written, err := write("cas", "t1", 1, "one", now+1)
			require.NoError(t, err)
			require.Equal(t, int64(2), written.Sequence)
			require.Equal(t, now+1000, written.Expires)

			_, err = write("cas", "t2", 1, "stale", now+2)
			require.ErrorIs(t, err, api.ErrRendezvousConcurrentWrite)

			session, err := db.GetRendezvousSession(ctx, "cas", api.RendezvousKindMSC4388, now+3)
			require.NoError(t, err)
			require.Equal(t, "one", string(session.Data))
			require.Equal(t, int64(2), session.Sequence)
			require.Equal(t, now+1, session.LastModified)

			// A retried transaction gets its recorded answer and writes nothing.
			written, err = write("cas", "t1", 1, "ignored", now+4)
			require.NoError(t, err)
			require.Equal(t, int64(2), written.Sequence)
			sequence, found, err := db.GetRendezvousTxn(ctx, "cas", api.RendezvousKindMSC4388, "t1", now+4)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, int64(2), sequence)
			session, err = db.GetRendezvousSession(ctx, "cas", api.RendezvousKindMSC4388, now+5)
			require.NoError(t, err)
			require.Equal(t, "one", string(session.Data))
		})

		t.Run("transaction IDs are capped", func(t *testing.T) {
			newSession("cap", api.RendezvousKindMSC4388, now+1000)
			for i := range 5 {
				_, err := write("cap", fmt.Sprintf("t%d", i), int64(i+1), "x", now)
				require.NoError(t, err)
			}
			for i := range 5 {
				_, found, err := db.GetRendezvousTxn(ctx, "cap", api.RendezvousKindMSC4388, fmt.Sprintf("t%d", i), now)
				require.NoError(t, err)
				// MaxTxns is 3: the two oldest are forgotten.
				require.Equal(t, i >= 2, found, "t%d", i)
			}
		})

		t.Run("kinds are separate", func(t *testing.T) {
			newSession("kind", api.RendezvousKindMSC4108, now+1000)
			_, err := db.GetRendezvousSession(ctx, "kind", api.RendezvousKindMSC4388, now)
			require.ErrorIs(t, err, api.ErrRendezvousNotFound)
			_, err = write("kind", "t", 1, "x", now)
			require.ErrorIs(t, err, api.ErrRendezvousNotFound)
			deleted, err := db.DeleteRendezvousSession(ctx, "kind", api.RendezvousKindMSC4388, now)
			require.NoError(t, err)
			require.False(t, deleted)
		})

		t.Run("expiry, deletion and purge", func(t *testing.T) {
			newSession("exp", api.RendezvousKindMSC4388, now+100)
			_, err := write("exp", "t", 1, "x", now+1)
			require.NoError(t, err)

			_, err = db.GetRendezvousSession(ctx, "exp", api.RendezvousKindMSC4388, now+100)
			require.ErrorIs(t, err, api.ErrRendezvousNotFound)
			_, _, err = db.GetRendezvousTxn(ctx, "exp", api.RendezvousKindMSC4388, "t", now+100)
			require.ErrorIs(t, err, api.ErrRendezvousNotFound)
			_, err = write("exp", "t2", 2, "y", now+100)
			require.ErrorIs(t, err, api.ErrRendezvousNotFound)
			// A retry of a recorded transaction of an expired session fails too.
			_, err = write("exp", "t", 1, "x", now+100)
			require.ErrorIs(t, err, api.ErrRendezvousNotFound)

			require.NoError(t, db.PurgeRendezvousSessions(ctx, now+100))
			// The purge took the transaction IDs along: a new session with
			// the same ID does not inherit them.
			newSession("exp", api.RendezvousKindMSC4388, now+1000)
			_, found, err := db.GetRendezvousTxn(ctx, "exp", api.RendezvousKindMSC4388, "t", now+200)
			require.NoError(t, err)
			require.False(t, found)

			deleted, err := db.DeleteRendezvousSession(ctx, "exp", api.RendezvousKindMSC4388, now+200)
			require.NoError(t, err)
			require.True(t, deleted)
			deleted, err = db.DeleteRendezvousSession(ctx, "exp", api.RendezvousKindMSC4388, now+200)
			require.NoError(t, err)
			require.False(t, deleted)
		})

		t.Run("session limit", func(t *testing.T) {
			require.NoError(t, db.PurgeRendezvousSessions(ctx, now+100_000))
			later := now + 100_000
			create := func(id, userID string) error {
				return db.CreateRendezvousSession(ctx, &api.RendezvousSession{
					ID: id, Kind: api.RendezvousKindMSC4388, Data: []byte{},
					Sequence: 1, LastModified: later, Expires: later + 1000,
					Anonymous: userID == "", UserID: userID,
				}, 2, 2)
			}
			for i := range 2 {
				require.NoError(t, create(fmt.Sprintf("limit%d", i), ""))
			}
			require.ErrorIs(t, create("limit2", ""), api.ErrRendezvousLimit)
			// Users do not count towards the anonymous limit, and each has
			// their own.
			require.NoError(t, create("alice0", "@alice:test"))
			require.NoError(t, create("alice1", "@alice:test"))
			require.ErrorIs(t, create("alice2", "@alice:test"), api.ErrRendezvousLimit)
			require.NoError(t, create("bob0", "@bob:test"))
			require.ErrorIs(t, create("limit3", ""), api.ErrRendezvousLimit)
			session, err := db.GetRendezvousSession(ctx, "alice0", api.RendezvousKindMSC4388, later)
			require.NoError(t, err)
			require.False(t, session.Anonymous)
			require.Equal(t, "@alice:test", session.UserID)
			// Expired sessions no longer count.
			require.NoError(t, db.CreateRendezvousSession(ctx, &api.RendezvousSession{
				ID: "alice3", Kind: api.RendezvousKindMSC4388, Data: []byte{},
				Sequence: 1, LastModified: later + 1000, Expires: later + 2000, UserID: "@alice:test",
			}, 2, 2))
		})

		t.Run("concurrent writers", func(t *testing.T) {
			newSession("race", api.RendezvousKindMSC4388, now+1000)
			const writers = 8
			var wg sync.WaitGroup
			results := make([]error, writers)
			sequences := make([]int64, writers)
			for i := range writers {
				wg.Go(func() {
					// Half of the writers retry the same transaction.
					txnID := fmt.Sprintf("w%d", i)
					if i%2 == 0 {
						txnID = "shared"
					}
					written, err := write("race", txnID, 1, fmt.Sprintf("writer %d", i), now)
					results[i] = err
					if err == nil {
						sequences[i] = written.Sequence
					}
				})
			}
			wg.Wait()
			// Exactly one write lands; retries of its transaction, if it was
			// the shared one, get its answer, and everyone else conflicts.
			session, err := db.GetRendezvousSession(ctx, "race", api.RendezvousKindMSC4388, now)
			require.NoError(t, err)
			require.Equal(t, int64(2), session.Sequence)
			var winner int
			_, err = fmt.Sscanf(string(session.Data), "writer %d", &winner)
			require.NoError(t, err)
			for i := range writers {
				sameTxn := i == winner || (i%2 == 0 && winner%2 == 0)
				if sameTxn {
					require.NoError(t, results[i], "writer %d", i)
					require.Equal(t, int64(2), sequences[i], "writer %d", i)
				} else {
					require.ErrorIs(t, results[i], api.ErrRendezvousConcurrentWrite, "writer %d", i)
				}
			}
		})
	})
}
