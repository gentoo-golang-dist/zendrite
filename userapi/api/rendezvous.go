// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package api

import (
	"context"
	"errors"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
)

// RendezvousKind names the API a rendezvous session was created through.
// A session is only visible through the API that created it.
type RendezvousKind string

const (
	// RendezvousKindMSC4388 is a session of the MSC4388 rendezvous API,
	// which exchanges JSON and compares sequence tokens.
	RendezvousKindMSC4388 RendezvousKind = "msc4388"
	// RendezvousKindMSC4108 is a session of the 2024 MSC4108 rendezvous API,
	// which exchanges text/plain bodies and compares ETags.
	RendezvousKindMSC4108 RendezvousKind = "msc4108"
)

var (
	// ErrRendezvousNotFound is returned for a session that does not exist,
	// has expired, was deleted or belongs to the other rendezvous API.
	ErrRendezvousNotFound = errors.New("rendezvous session not found")
	// ErrRendezvousConcurrentWrite is returned when a write names a sequence
	// that is no longer the session's current one.
	ErrRendezvousConcurrentWrite = errors.New("rendezvous session was written concurrently")
	// ErrRendezvousLimit is returned when too many sessions are active.
	ErrRendezvousLimit = errors.New("too many active rendezvous sessions")
)

// RendezvousSession is the shared payload of a rendezvous session used by QR
// code login (MSC4108 over MSC4388). Sessions are stored in the database so
// that every replica of an HA deployment serves the same state.
type RendezvousSession struct {
	ID   string
	Kind RendezvousKind
	Data []byte
	// ContentType is the media type of Data for MSC4108 sessions, and empty
	// for MSC4388 sessions.
	ContentType string
	// Sequence starts at 1 and grows by one with every successful write. It
	// is the MSC4388 sequence_token and the MSC4108 ETag.
	Sequence     int64
	LastModified spec.Timestamp
	Expires      spec.Timestamp
	// Anonymous is set for sessions created without an access token, or by
	// a guest or suspended account. Only they count towards the global
	// session limit, so anonymous callers cannot lock signed-in users out.
	Anonymous bool
	// UserID is the user who created a session that is not anonymous. Each
	// user has a limit of their own.
	UserID string
}

// RendezvousWrite replaces the payload of a session, if its sequence is
// still ExpectedSequence.
type RendezvousWrite struct {
	ID               string
	Kind             RendezvousKind
	ExpectedSequence int64
	Data             []byte
	ContentType      string
	// TxnID, when set, records the new sequence against the transaction ID
	// so that a retry gets the same answer (MSC4388). A write whose TxnID is
	// already recorded returns the recorded sequence without writing.
	TxnID string
	// MaxTxns bounds the transaction IDs remembered per session; the oldest
	// are forgotten first.
	MaxTxns int
	Now     spec.Timestamp
}

// RendezvousAPI stores rendezvous sessions for QR code login.
type RendezvousAPI interface {
	// PerformRendezvousCreate stores a new session. It fails with
	// ErrRendezvousLimit when the session is anonymous and maxAnonymous
	// unexpired anonymous sessions exist, or when it is not and its user has
	// maxPerUser unexpired sessions (0 means no limit).
	PerformRendezvousCreate(ctx context.Context, session *RendezvousSession, maxAnonymous, maxPerUser int) error
	// QueryRendezvousSession returns an unexpired session, or
	// ErrRendezvousNotFound.
	QueryRendezvousSession(ctx context.Context, id string, kind RendezvousKind, now spec.Timestamp) (*RendezvousSession, error)
	// QueryRendezvousTxn returns the sequence recorded for a transaction ID
	// of an unexpired session, and whether one was recorded.
	QueryRendezvousTxn(ctx context.Context, id string, kind RendezvousKind, txnID string, now spec.Timestamp) (int64, bool, error)
	// PerformRendezvousWrite compares and swaps the payload of a session. It
	// returns the session as written, without Data, or ErrRendezvousNotFound
	// or ErrRendezvousConcurrentWrite.
	PerformRendezvousWrite(ctx context.Context, write *RendezvousWrite) (*RendezvousSession, error)
	// PerformRendezvousDelete deletes an unexpired session and reports
	// whether there was one.
	PerformRendezvousDelete(ctx context.Context, id string, kind RendezvousKind, now spec.Timestamp) (bool, error)
}
