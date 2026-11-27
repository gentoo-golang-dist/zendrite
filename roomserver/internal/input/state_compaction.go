// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/roomserver/storage/shared"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
)

const (
	// StateCompactionFirstDelay lets the server settle after it starts.
	stateCompactionFirstDelay = 5 * time.Minute
	// StateCompactionInterval is how often a pass runs until the compaction
	// has finished; a pass rewrites the blocks the previous one prepared.
	stateCompactionInterval = time.Hour
	// StateCompactionGrace is how long a prepared block waits before it is
	// rewritten, far longer than any request keeps a list of blocks.
	stateCompactionGrace = 10 * time.Minute
	// StateCompactionMinEntries is the smallest full block worth re-encoding.
	stateCompactionMinEntries = 1000
)

func (r *Inputer) compactStateBlocks() {
	ctx := r.ProcessContext.Context()
	owns := func() bool { return jetstream.OwnsHomeserver(r.JetStream) }
	opts := shared.StateCompactionOptions{
		MinEntries: stateCompactionMinEntries,
		Grace:      stateCompactionGrace,
		Now:        time.Now,
	}
	RunStateCompaction(ctx, r.DB, opts, owns, stateCompactionFirstDelay, stateCompactionInterval)
}

// StateCompactionDatabase is the storage the state compaction runs on.
type StateCompactionDatabase interface {
	StateCompactionDone(ctx context.Context) (bool, error)
	FinishStateCompaction(ctx context.Context) error
	StateCompactionRoomNIDs(ctx context.Context) ([]types.RoomNID, error)
	PrepareStateCompaction(ctx context.Context, roomNID types.RoomNID, opts shared.StateCompactionOptions, owns func() bool) (shared.StateCompactionResult, error)
	RewriteStateCompactions(ctx context.Context, opts shared.StateCompactionOptions, owns func() bool) (shared.StateCompactionResult, error)
}

// RunStateCompaction runs state compaction passes, first after firstDelay and
// then every interval while this process owns the homeserver, until a pass
// finds nothing left to do.
func RunStateCompaction(
	ctx context.Context, db StateCompactionDatabase, opts shared.StateCompactionOptions,
	owns func() bool, firstDelay, interval time.Duration,
) {
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if owns() {
			done, err := StateCompactionPass(ctx, db, opts, owns)
			if err != nil {
				logrus.WithError(err).Warn("State compaction: pass failed")
			}
			if done {
				return
			}
		}
		timer.Reset(interval)
	}
}

// StateCompactionPass rewrites the blocks that earlier passes prepared, then
// prepares the rest of every room. It reports true once the compaction has
// finished for good, which it records so that no later start scans again.
func StateCompactionPass(ctx context.Context, db StateCompactionDatabase, opts shared.StateCompactionOptions, owns func() bool) (bool, error) {
	log := logrus.WithField("component", "roomserver")
	if done, err := db.StateCompactionDone(ctx); err != nil || done {
		return done, err
	}
	start := time.Now()
	var result shared.StateCompactionResult
	rewrite, err := db.RewriteStateCompactions(ctx, opts, owns)
	addResult(&result, rewrite)
	if err != nil {
		return false, err
	}
	roomNIDs, err := db.StateCompactionRoomNIDs(ctx)
	if err != nil {
		return false, err
	}
	for _, roomNID := range roomNIDs {
		if ctx.Err() != nil || !owns() {
			return false, ctx.Err()
		}
		prepared, err := db.PrepareStateCompaction(ctx, roomNID, opts, owns)
		addResult(&result, prepared)
		if err != nil {
			log.WithError(err).WithField("room_nid", roomNID).Warn("State compaction: failed to prepare the room")
			result.Retry++
		}
	}
	log.WithFields(logrus.Fields{
		"prepared": result.Prepared, "rewritten": result.Rewritten, "waiting": result.Waiting,
		"abandoned": result.Abandoned, "retry": result.Retry, "duration": time.Since(start),
	}).Info("State compaction: pass finished")
	if result.Prepared > 0 || result.Waiting > 0 || result.Retry > 0 || !owns() {
		return false, nil
	}
	if err = db.FinishStateCompaction(ctx); err != nil {
		return false, err
	}
	log.Info("State compaction: finished, old full state blocks are re-encoded")
	return true, nil
}

func addResult(total *shared.StateCompactionResult, r shared.StateCompactionResult) {
	total.Prepared += r.Prepared
	total.Rewritten += r.Rewritten
	total.Waiting += r.Waiting
	total.Abandoned += r.Abandoned
	total.Retry += r.Retry
}
