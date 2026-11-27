// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/roomserver/storage/tables"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// The state block compaction re-encodes state blocks that hold the full state
// of a room, written before merged state was stored as a delta, as their
// differences from an earlier full block of the same room, the base.
//
// A snapshot combines its blocks in order, a later block overriding an earlier
// one for the same key, and keeps its blocks sorted by NID. A block X whose
// state contains every key of an earlier block B can therefore be replaced by
// its differences from B in every snapshot that holds B just before X. The
// compaction gets there in two steps, each one transaction, so that every
// committed state of the database, and every crash or dropped connection in
// between, leaves every snapshot with the state it had:
//
//  1. Prepare: record the plan, and add B to every snapshot that starts with
//     X. X still holds its full state and overrides B, so no state changes.
//  2. Rewrite: once no reader or writer can still hold a block list read
//     before step 1, check that every snapshot holding X holds B just before
//     it, replace the events of X with its differences from B, and verify
//     that B and the new X combine to exactly the old X.
//
// Snapshot and block updates are guarded by their old hashes, and a plan whose
// conditions no longer hold is abandoned, which leaves X full. Writers add the
// base of every compacted block to the snapshots they create, see
// addCompactionBases.

// StateCompactionOptions controls the state block compaction.
type StateCompactionOptions struct {
	// MinEntries is the size from which a block is worth re-encoding.
	MinEntries int
	// Grace is how long after a plan is prepared its block may be rewritten:
	// longer than any reader keeps a list of blocks before it reads them.
	Grace time.Duration
	// Now returns the current time.
	Now func() time.Time
	// Hook runs at each step for tests, which fail the step by returning an
	// error, as a dropped connection would.
	Hook func(ctx context.Context, txn *sql.Tx, step string) error
}

// StateCompactionResult counts what a compaction call did.
type StateCompactionResult struct {
	Prepared  int
	Rewritten int
	// Waiting counts plans whose grace period or open transactions keep
	// them from being rewritten yet.
	Waiting   int
	Abandoned int
	// Retry counts blocks left for a later pass because their snapshots
	// changed meanwhile.
	Retry int
}

func (r *StateCompactionResult) add(o StateCompactionResult) {
	r.Prepared += o.Prepared
	r.Rewritten += o.Rewritten
	r.Waiting += o.Waiting
	r.Abandoned += o.Abandoned
	r.Retry += o.Retry
}

// The compaction steps that StateCompactionOptions.Hook sees.
const (
	StateCompactionStepPrepare         = "prepare"
	StateCompactionStepPrepareSnapshot = "prepare_snapshot"
	StateCompactionStepPrepareCommit   = "prepare_commit"
	StateCompactionStepRewrite         = "rewrite"
	StateCompactionStepRewriteBlock    = "rewrite_block"
	StateCompactionStepRewriteCommit   = "rewrite_commit"
)

var (
	// ErrCompactionAbandon ends a plan for good, leaving its block full.
	errCompactionAbandon = errors.New("state compaction: plan abandoned")
	// ErrCompactionRetry leaves a block for a later pass.
	errCompactionRetry = errors.New("state compaction: retry later")
)

func (o StateCompactionOptions) hook(ctx context.Context, txn *sql.Tx, step string) error {
	if o.Hook == nil {
		return nil
	}
	return o.Hook(ctx, txn, step)
}

// compactionBlock is a block of full room state as the compaction reads it.
type compactionBlock struct {
	nid types.StateBlockNID
	// stored is the block's events in the order the block stores them.
	stored types.EventNIDs
	state  map[types.StateKeyTuple]types.EventNID
}

// loadCompactionBlock reads a block and maps it to state keys. It returns nil
// if the block is gone, or if an event of it no longer resolves or two of its
// events share a key, which the compaction cannot re-encode.
func (d *Database) loadCompactionBlock(ctx context.Context, txn *sql.Tx, nid types.StateBlockNID) (*compactionBlock, error) {
	stored, err := d.StateBlockTable.BulkSelectStateBlockEntries(ctx, txn, types.StateBlockNIDs{nid})
	if err != nil || len(stored) != 1 {
		return nil, nil //nolint:nilerr // a missing block ends the plan, not the pass
	}
	unique := slices.Clone(stored[0])
	slices.Sort(unique)
	if len(slices.Compact(unique)) != len(stored[0]) {
		return nil, nil
	}
	entries, err := d.EventsTable.BulkSelectStateEventByNID(ctx, txn, stored[0], nil)
	if err != nil {
		return nil, fmt.Errorf("d.EventsTable.BulkSelectStateEventByNID: %w", err)
	}
	if len(entries) != len(stored[0]) {
		return nil, nil
	}
	state := make(map[types.StateKeyTuple]types.EventNID, len(entries))
	for _, entry := range entries {
		if _, dup := state[entry.StateKeyTuple]; dup {
			return nil, nil
		}
		state[entry.StateKeyTuple] = entry.EventNID
	}
	return &compactionBlock{nid: nid, stored: stored[0], state: state}, nil
}

// compactionDelta returns the entries of block that differ from base, and
// false if base has a key that block lacks: a later block cannot remove one.
func compactionDelta(base, block *compactionBlock) (types.StateEntries, bool) {
	for key := range base.state {
		if _, ok := block.state[key]; !ok {
			return nil, false
		}
	}
	var delta types.StateEntries
	for key, event := range block.state {
		if base.state[key] != event {
			delta = append(delta, types.StateEntry{StateKeyTuple: key, EventNID: event})
		}
	}
	return delta, true
}

// worthCompacting reports whether a delta is small enough to store instead of
// the full block.
func worthCompacting(delta types.StateEntries, block *compactionBlock) bool {
	return len(delta)*maxCompactionDeltaDivisor <= len(block.state)
}

// maxCompactionDeltaDivisor bounds a delta as the room server does for new
// state: at most a quarter of the block.
const maxCompactionDeltaDivisor = 4

// storedEvents returns the events of entries in the order a block stores
// them, so that their hash matches the one BulkInsertStateData computes.
func storedEvents(entries types.StateEntries) types.EventNIDs {
	entries = slices.Clone(entries)
	entries = entries[:util.SortAndUnique(entries)]
	events := make(types.EventNIDs, len(entries))
	for i, entry := range entries {
		events[i] = entry.EventNID
	}
	return events
}

// withBase returns blocks with base added, sorted as snapshots store them,
// and whether base then lies just before block.
func withBase(blocks []types.StateBlockNID, base, block types.StateBlockNID) (types.StateBlockNIDs, bool) {
	out := types.StateBlockNIDs(slices.Clone(blocks))
	if !slices.Contains(out, base) {
		out = append(out, base)
	}
	out = out[:util.SortAndUnique(out)]
	i := slices.Index(out, block)
	return out, i > 0 && out[i-1] == base
}

// StateCompactionDone reports whether the compaction has finished for good.
func (d *Database) StateCompactionDone(ctx context.Context) (bool, error) {
	return d.StateCompactionTable.SelectDone(ctx, nil)
}

// FinishStateCompaction records that the compaction has finished for good.
func (d *Database) FinishStateCompaction(ctx context.Context) error {
	return d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		return d.StateCompactionTable.InsertDone(ctx, txn)
	})
}

// StateCompactionRoomNIDs returns the rooms that have state snapshots.
func (d *Database) StateCompactionRoomNIDs(ctx context.Context) ([]types.RoomNID, error) {
	return d.StateCompactionTable.SelectRoomNIDs(ctx, nil)
}

// PrepareStateCompaction plans the re-encoding of the room's full blocks and
// prepares each plan. It considers only blocks that every snapshot holding
// them starts with, that the room's current state does not use, and that are
// not the base of a plan. Going through them in order, it keeps a block full
// as the base for the next ones unless it can be stored as a small delta
// against the current base.
func (d *Database) PrepareStateCompaction(
	ctx context.Context, roomNID types.RoomNID, opts StateCompactionOptions, owns func() bool,
) (StateCompactionResult, error) {
	var result StateCompactionResult
	snapshots, err := d.StateCompactionTable.SelectRoomSnapshots(ctx, nil, roomNID)
	if err != nil {
		return result, fmt.Errorf("d.StateCompactionTable.SelectRoomSnapshots: %w", err)
	}
	plans, err := d.StateCompactionTable.SelectRoomPlans(ctx, nil, roomNID)
	if err != nil {
		return result, fmt.Errorf("d.StateCompactionTable.SelectRoomPlans: %w", err)
	}
	skip := map[types.StateBlockNID]bool{}
	var planBases []types.StateBlockNID
	for _, plan := range plans {
		skip[plan.BlockNID] = true
		if plan.State != tables.StateCompactionAbandoned {
			skip[plan.BaseNID] = true
			planBases = append(planBases, plan.BaseNID)
		}
	}
	slices.Sort(planBases)
	planBases = slices.Compact(planBases)
	_, currentNID, err := d.RoomsTable.SelectLatestEventNIDs(ctx, nil, roomNID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, fmt.Errorf("d.RoomsTable.SelectLatestEventNIDs: %w", err)
	}
	first := map[types.StateBlockNID]bool{}
	for _, snapshot := range snapshots {
		for i, block := range snapshot.StateBlockNIDs {
			if i == 0 {
				if _, seen := first[block]; !seen {
					first[block] = true
				}
			} else {
				// A block that follows another one is a delta, or comes after
				// a base, and stays as it is.
				first[block] = false
			}
		}
		if snapshot.StateSnapshotNID == currentNID {
			for _, block := range snapshot.StateBlockNIDs {
				skip[block] = true
			}
		}
	}
	var candidates []types.StateBlockNID
	for block, onlyFirst := range first {
		if onlyFirst && !skip[block] {
			candidates = append(candidates, block)
		}
	}
	slices.Sort(candidates)

	var base *compactionBlock
	for _, nid := range candidates {
		if ctx.Err() != nil || !owns() {
			return result, ctx.Err()
		}
		block, err := d.loadCompactionBlock(ctx, nil, nid)
		if err != nil {
			return result, err
		}
		if block == nil || len(block.state) < opts.MinEntries {
			continue
		}
		// Carry on from the base of an earlier pass, as an uninterrupted pass
		// would have, when it is more recent than the current base.
		if i, _ := slices.BinarySearch(planBases, nid); i > 0 && (base == nil || planBases[i-1] > base.nid) {
			planBase, err := d.loadCompactionBlock(ctx, nil, planBases[i-1])
			if err != nil {
				return result, err
			}
			if planBase != nil {
				base = planBase
			}
		}
		if base != nil {
			if delta, ok := compactionDelta(base, block); ok && worthCompacting(delta, block) {
				switch err := d.prepareStateCompaction(ctx, roomNID, base.nid, nid, opts); {
				case err == nil:
					result.Prepared++
					continue
				case errors.Is(err, errCompactionRetry):
					result.Retry++
					continue
				case errors.Is(err, errCompactionAbandon):
					result.Abandoned++
				default:
					return result, err
				}
			}
		}
		base = block
	}
	return result, nil
}

// prepareStateCompaction records the plan to re-encode block against base and
// adds base to every snapshot that starts with block, in one transaction.
func (d *Database) prepareStateCompaction(
	ctx context.Context, roomNID types.RoomNID, baseNID, blockNID types.StateBlockNID, opts StateCompactionOptions,
) error {
	plan := tables.StateCompactionPlan{
		BlockNID: blockNID, RoomNID: roomNID, BaseNID: baseNID,
		PreparedAt: opts.Now().UnixMilli(), State: tables.StateCompactionPrepared,
	}
	err := d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		if err := opts.hook(ctx, txn, StateCompactionStepPrepare); err != nil {
			return err
		}
		inserted, err := d.StateCompactionTable.InsertPlan(ctx, txn, plan)
		if err != nil {
			return fmt.Errorf("d.StateCompactionTable.InsertPlan: %w", err)
		}
		if !inserted {
			return errCompactionRetry
		}
		snapshots, err := d.StateCompactionTable.SelectSnapshotsWithBlock(ctx, txn, roomNID, blockNID)
		if err != nil {
			return fmt.Errorf("d.StateCompactionTable.SelectSnapshotsWithBlock: %w", err)
		}
		for _, snapshot := range snapshots {
			if len(snapshot.StateBlockNIDs) == 0 || snapshot.StateBlockNIDs[0] != blockNID {
				return errCompactionAbandon
			}
			if err = d.addCompactionBase(ctx, txn, snapshot, baseNID, blockNID); err != nil {
				return err
			}
			if err = opts.hook(ctx, txn, StateCompactionStepPrepareSnapshot); err != nil {
				return err
			}
		}
		return opts.hook(ctx, txn, StateCompactionStepPrepareCommit)
	})
	if errors.Is(err, errCompactionAbandon) {
		plan.State = tables.StateCompactionAbandoned
		if abandonErr := d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
			_, err := d.StateCompactionTable.InsertPlan(ctx, txn, plan)
			return err
		}); abandonErr != nil {
			return abandonErr
		}
	}
	return err
}

// addCompactionBase adds base to a snapshot that holds block, so that it lies
// just before block. It returns errCompactionAbandon if it cannot, and
// errCompactionRetry if the snapshot changed meanwhile.
func (d *Database) addCompactionBase(
	ctx context.Context, txn *sql.Tx, snapshot types.StateBlockNIDList, baseNID, blockNID types.StateBlockNID,
) error {
	blocks, adjacent := withBase(snapshot.StateBlockNIDs, baseNID, blockNID)
	if !adjacent {
		return errCompactionAbandon
	}
	if slices.Equal(blocks, snapshot.StateBlockNIDs) {
		return nil
	}
	// Two snapshots cannot hold the same blocks.
	if exists, err := d.StateCompactionTable.SnapshotExists(ctx, txn, blocks); err != nil || exists {
		if err != nil {
			return fmt.Errorf("d.StateCompactionTable.SnapshotExists: %w", err)
		}
		return errCompactionAbandon
	}
	updated, err := d.StateCompactionTable.UpdateSnapshotBlocks(ctx, txn, snapshot.StateSnapshotNID, snapshot.StateBlockNIDs, blocks)
	if err != nil {
		return fmt.Errorf("d.StateCompactionTable.UpdateSnapshotBlocks: %w", err)
	}
	if !updated {
		return errCompactionRetry
	}
	return nil
}

// RewriteStateCompactions rewrites the blocks of prepared plans once no
// reader or writer can hold a block list read before they were prepared.
func (d *Database) RewriteStateCompactions(
	ctx context.Context, opts StateCompactionOptions, owns func() bool,
) (StateCompactionResult, error) {
	var result StateCompactionResult
	plans, err := d.StateCompactionTable.SelectPlans(ctx, nil, tables.StateCompactionPrepared)
	if err != nil {
		return result, fmt.Errorf("d.StateCompactionTable.SelectPlans: %w", err)
	}
	oldest, open, err := d.StateCompactionTable.SelectOldestTransactionStart(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("d.StateCompactionTable.SelectOldestTransactionStart: %w", err)
	}
	for _, plan := range plans {
		if ctx.Err() != nil || !owns() {
			return result, ctx.Err()
		}
		now := opts.Now()
		if now.Sub(time.UnixMilli(plan.PreparedAt)) < opts.Grace || (open && oldest <= plan.PreparedAt) {
			result.Waiting++
			continue
		}
		res, err := d.rewriteStateCompaction(ctx, plan, opts)
		result.add(res)
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

// rewriteStateCompaction replaces the block of a prepared plan with its
// differences from the base, in one transaction that checks every condition
// again and verifies the result before it commits.
func (d *Database) rewriteStateCompaction(
	ctx context.Context, plan tables.StateCompactionPlan, opts StateCompactionOptions,
) (StateCompactionResult, error) {
	var result StateCompactionResult
	err := d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		result = StateCompactionResult{}
		if err := opts.hook(ctx, txn, StateCompactionStepRewrite); err != nil {
			return err
		}
		locked, err := d.StateCompactionTable.LockPlan(ctx, txn, plan.BlockNID)
		if err != nil {
			return fmt.Errorf("d.StateCompactionTable.LockPlan: %w", err)
		}
		if locked == nil || *locked != plan {
			// Another pass got to it, or prepared it again.
			return nil
		}
		abandon := func() error {
			result.Abandoned++
			return d.StateCompactionTable.UpdatePlan(ctx, txn, plan.BlockNID, tables.StateCompactionAbandoned, plan.PreparedAt)
		}

		// Every snapshot holding the block must hold the base just before it.
		snapshots, err := d.StateCompactionTable.SelectSnapshotsWithBlock(ctx, txn, plan.RoomNID, plan.BlockNID)
		if err != nil {
			return fmt.Errorf("d.StateCompactionTable.SelectSnapshotsWithBlock: %w", err)
		}
		added := false
		for _, snapshot := range snapshots {
			if _, adjacent := withBase(snapshot.StateBlockNIDs, plan.BaseNID, plan.BlockNID); adjacent && slices.Contains(snapshot.StateBlockNIDs, plan.BaseNID) {
				continue
			}
			// A snapshot written from a block list read before the plan was
			// prepared: prepare it too and wait again.
			switch err = d.addCompactionBase(ctx, txn, snapshot, plan.BaseNID, plan.BlockNID); {
			case err == nil:
				added = true
			case errors.Is(err, errCompactionAbandon):
				return abandon()
			default:
				return err
			}
		}
		if added {
			result.Waiting++
			return d.StateCompactionTable.UpdatePlan(ctx, txn, plan.BlockNID, tables.StateCompactionPrepared, opts.Now().UnixMilli())
		}

		block, err := d.loadCompactionBlock(ctx, txn, plan.BlockNID)
		if err != nil {
			return err
		}
		base, err := d.loadCompactionBlock(ctx, txn, plan.BaseNID)
		if err != nil {
			return err
		}
		if block == nil || base == nil {
			return abandon()
		}
		delta, ok := compactionDelta(base, block)
		if !ok || !worthCompacting(delta, block) {
			return abandon()
		}
		events, err := d.uniqueCompactionEvents(ctx, txn, base, delta)
		if err != nil {
			return err
		}
		if events == nil {
			return abandon()
		}
		updated, err := d.StateCompactionTable.UpdateBlockEvents(ctx, txn, plan.BlockNID, block.stored, events)
		if err != nil {
			return fmt.Errorf("d.StateCompactionTable.UpdateBlockEvents: %w", err)
		}
		if !updated {
			return abandon()
		}
		if err = opts.hook(ctx, txn, StateCompactionStepRewriteBlock); err != nil {
			return err
		}
		if err = d.verifyStateCompaction(ctx, txn, plan, base, block); err != nil {
			return err
		}
		if err = d.StateCompactionTable.UpdatePlan(ctx, txn, plan.BlockNID, tables.StateCompactionRewritten, plan.PreparedAt); err != nil {
			return fmt.Errorf("d.StateCompactionTable.UpdatePlan: %w", err)
		}
		result.Rewritten++
		return opts.hook(ctx, txn, StateCompactionStepRewriteCommit)
	})
	if errors.Is(err, errCompactionRetry) {
		return StateCompactionResult{Retry: 1}, nil
	}
	if err != nil {
		return StateCompactionResult{}, err
	}
	return result, nil
}

// uniqueCompactionEvents returns the events of delta in stored order, padded
// if another block already holds exactly them: blocks are unique by content,
// and a small delta often equals the block its one event was first stored in.
// Padding adds entries of the base, which change nothing because the base
// holds them already. It returns nil if no padding makes the block unique.
func (d *Database) uniqueCompactionEvents(
	ctx context.Context, txn *sql.Tx, base *compactionBlock, delta types.StateEntries,
) (types.EventNIDs, error) {
	var padding types.StateEntries
	for key, event := range base.state {
		if !slices.ContainsFunc(delta, func(e types.StateEntry) bool { return e.StateKeyTuple == key }) {
			padding = append(padding, types.StateEntry{StateKeyTuple: key, EventNID: event})
		}
		if len(padding) == maxCompactionPadding {
			break
		}
	}
	// Map iteration is random; pad deterministically.
	slices.SortFunc(padding, func(a, b types.StateEntry) int {
		if a.LessThan(b) {
			return -1
		}
		return 1
	})
	entries := slices.Clone(delta)
	for i := 0; ; i++ {
		events := storedEvents(entries)
		exists, err := d.StateCompactionTable.BlockExists(ctx, txn, events)
		if err != nil {
			return nil, fmt.Errorf("d.StateCompactionTable.BlockExists: %w", err)
		}
		if !exists {
			return events, nil
		}
		if i == len(padding) {
			return nil, nil
		}
		entries = append(entries, padding[i])
	}
}

const maxCompactionPadding = 4

// verifyStateCompaction reads the rewritten block back and checks that the
// base and it combine to exactly the block's old state, and that every
// snapshot holding it holds the base just before it.
func (d *Database) verifyStateCompaction(
	ctx context.Context, txn *sql.Tx, plan tables.StateCompactionPlan, base, old *compactionBlock,
) error {
	stored, err := d.StateBlockTable.BulkSelectStateBlockEntries(ctx, txn, types.StateBlockNIDs{plan.BlockNID})
	if err != nil {
		return fmt.Errorf("verify: d.StateBlockTable.BulkSelectStateBlockEntries: %w", err)
	}
	entries, err := d.EventsTable.BulkSelectStateEventByNID(ctx, txn, stored[0], nil)
	if err != nil {
		return fmt.Errorf("verify: d.EventsTable.BulkSelectStateEventByNID: %w", err)
	}
	if len(entries) != len(stored[0]) {
		return fmt.Errorf("verify: block %d has %d events but %d resolve", plan.BlockNID, len(stored[0]), len(entries))
	}
	combined := make(map[types.StateKeyTuple]types.EventNID, len(old.state))
	for key, event := range base.state {
		combined[key] = event
	}
	for _, entry := range entries {
		combined[entry.StateKeyTuple] = entry.EventNID
	}
	if len(combined) != len(old.state) {
		return fmt.Errorf("verify: block %d combines to %d entries, want %d", plan.BlockNID, len(combined), len(old.state))
	}
	for key, event := range old.state {
		if combined[key] != event {
			return fmt.Errorf("verify: block %d combines to event %d for %v, want %d", plan.BlockNID, combined[key], key, event)
		}
	}
	snapshots, err := d.StateCompactionTable.SelectSnapshotsWithBlock(ctx, txn, plan.RoomNID, plan.BlockNID)
	if err != nil {
		return fmt.Errorf("verify: d.StateCompactionTable.SelectSnapshotsWithBlock: %w", err)
	}
	for _, snapshot := range snapshots {
		i := slices.Index(snapshot.StateBlockNIDs, plan.BlockNID)
		if i < 1 || snapshot.StateBlockNIDs[i-1] != plan.BaseNID {
			return fmt.Errorf("verify: snapshot %d holds block %d without its base %d just before it", snapshot.StateSnapshotNID, plan.BlockNID, plan.BaseNID)
		}
	}
	return nil
}

// addCompactionBases adds to blocks the base of every compacted block in it,
// for a snapshot about to be written. A writer may have read the blocks
// before the compaction prepared one of them; without its base a compacted
// block holds only its differences. It reports false if a base would not lie
// just before its block, in which case the snapshot must hold its full state
// in one block instead.
func (d *Database) addCompactionBases(
	ctx context.Context, txn *sql.Tx, blocks []types.StateBlockNID,
) ([]types.StateBlockNID, bool, error) {
	if d.StateCompactionTable == nil || len(blocks) == 0 {
		return blocks, true, nil
	}
	bases, err := d.StateCompactionTable.SelectBases(ctx, txn, blocks)
	if err != nil {
		return nil, false, fmt.Errorf("d.StateCompactionTable.SelectBases: %w", err)
	}
	if len(bases) == 0 {
		return blocks, true, nil
	}
	out := slices.Clone(blocks)
	for block, base := range bases {
		var adjacent bool
		if out, adjacent = withBase(out, base, block); !adjacent {
			return blocks, false, nil
		}
	}
	// Adding one base must not separate another block from its own.
	for block, base := range bases {
		if i := slices.Index(out, block); i < 1 || out[i-1] != base {
			return blocks, false, nil
		}
	}
	return out, true, nil
}

// combineWithCompactionBases returns the full state of blocks followed by
// state, combining each compacted block after its base.
func (d *Database) combineWithCompactionBases(
	ctx context.Context, txn *sql.Tx, blocks []types.StateBlockNID, state []types.StateEntry,
) ([]types.StateEntry, error) {
	ordered := slices.Clone(blocks)
	// A base is never compacted itself, but follow chains anyway.
	for range maxCompactionBaseDepth {
		bases, err := d.StateCompactionTable.SelectBases(ctx, txn, ordered)
		if err != nil {
			return nil, fmt.Errorf("d.StateCompactionTable.SelectBases: %w", err)
		}
		var next []types.StateBlockNID
		changed := false
		for i, block := range ordered {
			if base, ok := bases[block]; ok && (i == 0 || ordered[i-1] != base) {
				next = append(next, base)
				changed = true
			}
			next = append(next, block)
		}
		ordered = next
		if !changed {
			return d.combineStateBlocks(ctx, txn, ordered, state)
		}
	}
	return nil, fmt.Errorf("state compaction: bases of blocks %v nest deeper than %d", blocks, maxCompactionBaseDepth)
}

const maxCompactionBaseDepth = 8
