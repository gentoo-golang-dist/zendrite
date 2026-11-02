// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared

import (
	"context"
	"database/sql"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/userapi/api"
)

// CreateRendezvousSession stores a new rendezvous session. An anonymous
// session is refused when maxAnonymous (when positive) unexpired anonymous
// sessions exist; any other session when its user has maxPerUser (when
// positive) unexpired sessions. Under HA two replicas may both pass a count
// at once, so a limit can be exceeded by a few.
func (d *Database) CreateRendezvousSession(ctx context.Context, session *api.RendezvousSession, maxAnonymous, maxPerUser int) error {
	return d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		var count, limit int
		var err error
		if session.Anonymous {
			limit = maxAnonymous
			if limit > 0 {
				count, err = d.Rendezvous.CountActiveRendezvousSessions(ctx, txn, session.LastModified)
			}
		} else {
			limit = maxPerUser
			if limit > 0 {
				count, err = d.Rendezvous.CountActiveUserRendezvousSessions(ctx, txn, session.UserID, session.LastModified)
			}
		}
		if err != nil {
			return err
		}
		if limit > 0 && count >= limit {
			return api.ErrRendezvousLimit
		}
		return d.Rendezvous.InsertRendezvousSession(ctx, txn, session)
	})
}

// GetRendezvousSession returns an unexpired session or api.ErrRendezvousNotFound.
func (d *Database) GetRendezvousSession(ctx context.Context, id string, kind api.RendezvousKind, now spec.Timestamp) (*api.RendezvousSession, error) {
	session, err := d.Rendezvous.SelectRendezvousSession(ctx, nil, id, kind, now)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, api.ErrRendezvousNotFound
	}
	return session, nil
}

// GetRendezvousTxn returns the sequence recorded for a transaction ID of an
// unexpired session, or api.ErrRendezvousNotFound for no such session.
func (d *Database) GetRendezvousTxn(ctx context.Context, id string, kind api.RendezvousKind, txnID string, now spec.Timestamp) (int64, bool, error) {
	if _, err := d.GetRendezvousSession(ctx, id, kind, now); err != nil {
		return 0, false, err
	}
	return d.Rendezvous.SelectRendezvousTxn(ctx, nil, id, txnID)
}

// WriteRendezvousSession compares and swaps the payload of a session in one
// transaction. On PostgreSQL the UPDATE takes the row lock, so of two
// concurrent writers naming the same sequence exactly one succeeds; the
// other sees the new sequence and fails, unless it retried the winner's
// transaction ID, in which case it gets the winner's answer.
func (d *Database) WriteRendezvousSession(ctx context.Context, write *api.RendezvousWrite) (*api.RendezvousSession, error) {
	var written *api.RendezvousSession
	err := d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		recorded := func() (bool, error) {
			if write.TxnID == "" {
				return false, nil
			}
			sequence, found, err := d.Rendezvous.SelectRendezvousTxn(ctx, txn, write.ID, write.TxnID)
			if err != nil || !found {
				return false, err
			}
			session, err := d.Rendezvous.SelectRendezvousSession(ctx, txn, write.ID, write.Kind, write.Now)
			if err != nil {
				return false, err
			}
			if session == nil {
				return false, api.ErrRendezvousNotFound
			}
			session.Data = nil
			session.Sequence = sequence
			written = session
			return true, nil
		}

		if ok, err := recorded(); ok || err != nil {
			return err
		}
		session, err := d.Rendezvous.UpdateRendezvousSession(ctx, txn, write.ID, write.Kind, write.ExpectedSequence, write.Data, write.ContentType, write.Now)
		if err != nil {
			return err
		}
		if session == nil {
			current, selectErr := d.Rendezvous.SelectRendezvousSession(ctx, txn, write.ID, write.Kind, write.Now)
			if selectErr != nil {
				return selectErr
			}
			if current == nil {
				return api.ErrRendezvousNotFound
			}
			// A concurrent retry of the same transaction may have won.
			if ok, recordErr := recorded(); ok || recordErr != nil {
				return recordErr
			}
			return api.ErrRendezvousConcurrentWrite
		}
		if write.TxnID != "" {
			if err = d.Rendezvous.InsertRendezvousTxn(ctx, txn, write.ID, write.TxnID, session.Sequence, session.Expires); err != nil {
				return err
			}
			// Every recorded transaction holds a distinct sequence, so
			// forgetting those at or below the newest minus MaxTxns keeps at
			// most MaxTxns of them.
			if write.MaxTxns > 0 && session.Sequence > int64(write.MaxTxns) {
				if err = d.Rendezvous.DeleteRendezvousTxnsUpTo(ctx, txn, write.ID, session.Sequence-int64(write.MaxTxns)); err != nil {
					return err
				}
			}
		}
		written = session
		return nil
	})
	if err != nil {
		return nil, err
	}
	return written, nil
}

// DeleteRendezvousSession deletes a session and reports whether it existed
// and had not expired.
func (d *Database) DeleteRendezvousSession(ctx context.Context, id string, kind api.RendezvousKind, now spec.Timestamp) (bool, error) {
	var deleted bool
	err := d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		var err error
		deleted, err = d.Rendezvous.DeleteRendezvousSession(ctx, txn, id, kind, now)
		return err
	})
	return deleted, err
}

// PurgeRendezvousSessions deletes expired sessions and their transaction IDs.
func (d *Database) PurgeRendezvousSessions(ctx context.Context, now spec.Timestamp) error {
	return d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		return d.Rendezvous.DeleteExpiredRendezvousSessions(ctx, txn, now)
	})
}
