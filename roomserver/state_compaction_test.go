// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/internal/input"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/storage"
	"codefloe.com/pat-s/zendrite/roomserver/storage/postgres"
	"codefloe.com/pat-s/zendrite/roomserver/storage/shared"
	"codefloe.com/pat-s/zendrite/roomserver/storage/sqlite3"
	"codefloe.com/pat-s/zendrite/roomserver/storage/tables"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// compactionFixture is a room whose history holds full state blocks that
// differ in a few members, as merges stored them before they were deltas.
type compactionFixture struct {
	db       storage.Database
	shared   *shared.Database
	roomInfo *types.RoomInfo
	dbType   test.DBType
	// full holds the snapshots that start with each synthetic full block.
	full []types.StateSnapshotNID
	// joins and renames are each member's two membership events.
	joins, renames []types.StateEntry
	// want is the state of every snapshot of the room before compaction.
	want map[types.StateSnapshotNID]map[types.StateKeyTuple]types.EventNID
}

// compactionMembers keeps the fixture small enough for -race in CI while
// each full block still holds more entries than the test's MinEntries.
const compactionMembers = 12

// compactionEnv is a room server and its database, which several fixtures
// can share: setting one up dominates the time of a fixture under -race.
type compactionEnv struct {
	db     storage.Database
	rsAPI  api.RoomserverInternalAPI
	dbType test.DBType
}

func newCompactionEnv(t *testing.T, dbType test.DBType) *compactionEnv {
	t.Helper()
	cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
	t.Cleanup(closeRig)
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(8*1024*1024, time.Hour, caching.DisableMetrics)
	db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches)
	require.NoError(t, err)
	natsInstance := jetstream.NATSInstance{}
	jsCtx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
	t.Cleanup(func() { jetstream.DeleteAllStreams(jsCtx, &cfg.Global.JetStream) })
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	return &compactionEnv{db: db, rsAPI: rsAPI, dbType: dbType}
}

func newCompactionFixture(t *testing.T, dbType test.DBType) *compactionFixture {
	t.Helper()
	return newCompactionFixtureIn(t, newCompactionEnv(t, dbType))
}

// newCompactionFixtureIn creates the fixture's room in env, and lets the
// compaction run again, which an earlier fixture of env may have finished.
func newCompactionFixtureIn(t *testing.T, env *compactionEnv) *compactionFixture {
	t.Helper()
	ctx := context.Background()
	db, rsAPI, dbType := env.db, env.rsAPI, env.dbType
	sh := sharedDatabase(t, db)
	require.NoError(t, sh.Writer.Do(sh.DB, nil, func(txn *sql.Tx) error {
		_, err := txn.ExecContext(ctx, "DELETE FROM roomserver_state_block_compaction WHERE block_nid = 0")
		return err
	}))

	alice := test.NewUser(t)
	room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
	events := room.Events()
	var joinIDs, renameIDs []string
	for range compactionMembers {
		user := test.NewUser(t)
		join := room.CreateAndInsert(t, user, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(user.ID))
		rename := room.CreateAndInsert(t, user, spec.MRoomMember, map[string]any{"membership": "join", "displayname": "renamed"}, test.WithStateKey(user.ID))
		events = append(events, join, rename)
		joinIDs = append(joinIDs, join.EventID())
		renameIDs = append(renameIDs, rename.EventID())
	}
	require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, events, "test", "test", "test", nil, false))

	f := &compactionFixture{db: db, shared: sh, dbType: dbType}
	var err error
	f.roomInfo, err = db.RoomInfo(ctx, room.ID)
	require.NoError(t, err)
	var created []string
	for _, ev := range room.Events()[:5] {
		created = append(created, ev.EventID())
	}
	createdEntries := entriesFor(ctx, t, db, created)
	f.joins = entriesFor(ctx, t, db, joinIDs)
	f.renames = entriesFor(ctx, t, db, renameIDs)

	// The full blocks, in order: small changes, a block lacking a key, a
	// large change, and one that restores an event an earlier block
	// overrode.
	members := slices.Clone(f.joins)
	fullStates := [][]types.StateEntry{}
	snapshot := func(changes map[int]types.StateEntry, drop int) []types.StateEntry {
		for i, entry := range changes {
			members[i] = entry
		}
		var out []types.StateEntry
		out = append(out, createdEntries...)
		for i, entry := range members {
			if i != drop {
				out = append(out, entry)
			}
		}
		return out
	}
	fullStates = append(fullStates,
		snapshot(nil, -1),
		snapshot(map[int]types.StateEntry{0: f.renames[0], 1: f.renames[1]}, -1),
		snapshot(map[int]types.StateEntry{2: f.renames[2], 3: f.renames[3]}, -1),
		snapshot(nil, compactionMembers-1),
		snapshot(map[int]types.StateEntry{4: f.renames[4], 5: f.renames[5]}, compactionMembers-1),
	)
	all := map[int]types.StateEntry{}
	for i := range compactionMembers {
		all[i] = f.renames[i]
	}
	fullStates = append(fullStates,
		snapshot(all, -1),
		snapshot(map[int]types.StateEntry{0: f.joins[0]}, -1),
	)
	for _, full := range fullStates {
		nid, err := db.AddState(ctx, f.roomInfo.RoomNID, nil, slices.Clone(full))
		require.NoError(t, err)
		f.full = append(f.full, nid)
		// A chain of deltas on top, as single_delta builds them.
		blocks := blockNIDs(ctx, t, db, nid)
		for i := range 2 {
			change := f.renames[10+i]
			if slices.Contains(full, change) {
				change = f.joins[10+i]
			}
			chained, err := db.AddState(ctx, f.roomInfo.RoomNID, blocks, []types.StateEntry{change})
			require.NoError(t, err)
			blocks = blockNIDs(ctx, t, db, chained)
		}
	}
	f.want = f.states(t)
	return f
}

func sharedDatabase(t *testing.T, db storage.Database) *shared.Database {
	t.Helper()
	switch d := db.(type) {
	case *postgres.Database:
		return &d.Database
	case *sqlite3.Database:
		return &d.Database
	}
	t.Fatalf("unknown database %T", db)
	return nil
}

func entriesFor(ctx context.Context, t *testing.T, db storage.Database, eventIDs []string) []types.StateEntry {
	t.Helper()
	nids, err := db.EventNIDs(ctx, eventIDs)
	require.NoError(t, err)
	entries, err := db.StateEntriesForEventIDs(ctx, eventIDs, true)
	require.NoError(t, err)
	byNID := map[types.EventNID]types.StateEntry{}
	for _, entry := range entries {
		byNID[entry.EventNID] = entry
	}
	out := make([]types.StateEntry, len(eventIDs))
	for i, id := range eventIDs {
		entry, ok := byNID[nids[id].EventNID]
		require.True(t, ok, "no state entry for %s", id)
		out[i] = entry
	}
	return out
}

func blockNIDs(ctx context.Context, t *testing.T, db storage.Database, nid types.StateSnapshotNID) []types.StateBlockNID {
	t.Helper()
	lists, err := db.StateBlockNIDs(ctx, []types.StateSnapshotNID{nid})
	require.NoError(t, err)
	return lists[0].StateBlockNIDs
}

// states loads the state of every snapshot of the room.
func (f *compactionFixture) states(t *testing.T) map[types.StateSnapshotNID]map[types.StateKeyTuple]types.EventNID {
	t.Helper()
	ctx := context.Background()
	snapshots, err := f.shared.StateCompactionTable.SelectRoomSnapshots(ctx, nil, f.roomInfo.RoomNID)
	require.NoError(t, err)
	resolver := state.NewStateResolution(f.db, f.roomInfo, nil)
	out := map[types.StateSnapshotNID]map[types.StateKeyTuple]types.EventNID{}
	for _, snapshot := range snapshots {
		entries, err := resolver.LoadStateAtSnapshot(ctx, snapshot.StateSnapshotNID)
		require.NoError(t, err)
		out[snapshot.StateSnapshotNID] = stateMapOf(entries)
	}
	return out
}

func stateMapOf(entries []types.StateEntry) map[types.StateKeyTuple]types.EventNID {
	out := make(map[types.StateKeyTuple]types.EventNID, len(entries))
	for _, entry := range entries {
		out[entry.StateKeyTuple] = entry.EventNID
	}
	return out
}

// requireUnchanged checks that every snapshot that existed before compaction
// still loads exactly the state it had.
func (f *compactionFixture) requireUnchanged(t *testing.T, when string) {
	t.Helper()
	got := f.states(t)
	for nid, want := range f.want {
		require.Contains(t, got, nid, "%s: snapshot %d is gone", when, nid)
		require.True(t, maps.Equal(want, got[nid]), "%s: snapshot %d changed state", when, nid)
	}
}

// storedEntries counts the events stored in the blocks of the room's
// snapshots.
func (f *compactionFixture) storedEntries(t *testing.T) int {
	t.Helper()
	ctx := context.Background()
	snapshots, err := f.shared.StateCompactionTable.SelectRoomSnapshots(ctx, nil, f.roomInfo.RoomNID)
	require.NoError(t, err)
	blocks := map[types.StateBlockNID]struct{}{}
	for _, snapshot := range snapshots {
		for _, block := range snapshot.StateBlockNIDs {
			blocks[block] = struct{}{}
		}
	}
	nids := slices.Sorted(maps.Keys(blocks))
	events, err := f.shared.StateBlockTable.BulkSelectStateBlockEntries(ctx, nil, nids)
	require.NoError(t, err)
	n := 0
	for _, e := range events {
		n += len(e)
	}
	return n
}

func (f *compactionFixture) plans(t *testing.T, state string) []tables.StateCompactionPlan {
	t.Helper()
	plans, err := f.shared.StateCompactionTable.SelectPlans(context.Background(), nil, state)
	require.NoError(t, err)
	return plans
}

func compactionOptions(hook func(context.Context, *sql.Tx, string) error) shared.StateCompactionOptions {
	return shared.StateCompactionOptions{MinEntries: 10, Grace: 0, Now: time.Now, Hook: hook}
}

func owned() bool { return true }

// runToCompletion runs passes until the compaction finishes, checking every
// snapshot after each one.
func (f *compactionFixture) runToCompletion(t *testing.T, opts shared.StateCompactionOptions) {
	t.Helper()
	for pass := range 10 {
		done, err := input.StateCompactionPass(context.Background(), f.db, opts, owned)
		require.NoError(t, err)
		f.requireUnchanged(t, fmt.Sprintf("after pass %d", pass))
		if done {
			return
		}
	}
	t.Fatal("the compaction did not finish in 10 passes")
}

// The synthetic full blocks that a compaction should rewrite: those after
// the first one of each run of small changes.
func (f *compactionFixture) expectRewritten(t *testing.T) {
	t.Helper()
	rewritten := map[types.StateBlockNID]bool{}
	for _, plan := range f.plans(t, tables.StateCompactionRewritten) {
		rewritten[plan.BlockNID] = true
	}
	for i, nid := range f.full {
		block := blockNIDs(context.Background(), t, f.db, nid)
		want := i == 1 || i == 2 || i == 4 || i == 6
		require.Equal(t, want, rewritten[block[len(block)-1]], "full block %d rewritten", i)
	}
}

func TestStateCompactionKeepsEveryState(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		f := newCompactionFixture(t, dbType)
		before := f.storedEntries(t)
		f.runToCompletion(t, compactionOptions(nil))
		f.expectRewritten(t)
		after := f.storedEntries(t)
		require.Less(t, after, before*2/3, "stored %d entries, %d before", after, before)

		done, err := f.db.StateCompactionDone(context.Background())
		require.NoError(t, err)
		require.True(t, done)
		// A finished compaction never scans again.
		res, err := f.db.PrepareStateCompaction(context.Background(), f.roomInfo.RoomNID, compactionOptions(nil), owned)
		require.NoError(t, err)
		require.Zero(t, res.Prepared)
		f.requireUnchanged(t, "after a pass on a finished compaction")
	})
}

var compactionSteps = []string{
	shared.StateCompactionStepPrepare,
	shared.StateCompactionStepPrepareSnapshot,
	shared.StateCompactionStepPrepareCommit,
	shared.StateCompactionStepRewrite,
	shared.StateCompactionStepRewriteBlock,
	shared.StateCompactionStepRewriteCommit,
}

var errInjected = errors.New("injected failure")

// A failure at any step, as a dropped connection or a crash causes it, leaves
// every snapshot with its state, and later passes finish the compaction.
func TestStateCompactionSurvivesFailureAtEveryStep(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		env := newCompactionEnv(t, dbType)
		for _, step := range compactionSteps {
			for _, occurrence := range []int{1, 2} {
				t.Run(fmt.Sprintf("%s/%d", step, occurrence), func(t *testing.T) {
					f := newCompactionFixtureIn(t, env)
					seen := 0
					failed := false
					hook := func(ctx context.Context, txn *sql.Tx, s string) error {
						if s != step || failed {
							return nil
						}
						seen++
						if seen < occurrence {
							return nil
						}
						failed = true
						if dbType == test.DBTypePostgres {
							// Kill the transaction's own connection, as a
							// network failure would.
							_, err := txn.ExecContext(ctx, "SELECT pg_terminate_backend(pg_backend_pid())")
							require.Error(t, err)
						}
						return errInjected
					}
					for range 3 {
						// A failed prepare is logged and retried by a later
						// pass; a failed rewrite ends the pass with its error.
						_, err := input.StateCompactionPass(context.Background(), f.db, compactionOptions(hook), owned)
						f.requireUnchanged(t, "after the failed pass")
						if failed {
							if step == shared.StateCompactionStepRewrite || step == shared.StateCompactionStepRewriteBlock || step == shared.StateCompactionStepRewriteCommit {
								require.Error(t, err)
							}
							break
						}
					}
					if !failed {
						t.Skipf("step %s occurs fewer than %d times", step, occurrence)
					}
					f.runToCompletion(t, compactionOptions(nil))
					f.expectRewritten(t)
				})
			}
		}
	})
}

// A writer that read a snapshot's blocks before the compaction prepared or
// rewrote one of them gets the base added to the snapshot it writes.
func TestStateCompactionStaleWriter(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		f := newCompactionFixture(t, dbType)
		stale := blockNIDs(ctx, t, f.db, f.full[1])
		require.Len(t, stale, 1)
		extra := f.renames[8]
		want := maps.Clone(f.want[f.full[1]])
		want[extra.StateKeyTuple] = extra.EventNID

		check := func(when string) {
			t.Helper()
			nid, err := f.db.AddState(ctx, f.roomInfo.RoomNID, slices.Clone(stale), []types.StateEntry{extra})
			require.NoError(t, err)
			resolver := state.NewStateResolution(f.db, f.roomInfo, nil)
			entries, err := resolver.LoadStateAtSnapshot(ctx, nid)
			require.NoError(t, err)
			require.True(t, maps.Equal(want, stateMapOf(entries)), "%s: a snapshot from a stale block list has the wrong state", when)
			f.want[nid] = want
		}

		check("before compaction")
		res, err := input.StateCompactionPass(ctx, f.db, compactionOptions(nil), owned)
		require.NoError(t, err)
		require.False(t, res)
		require.NotEmpty(t, f.plans(t, tables.StateCompactionPrepared))
		check("after prepare")
		f.requireUnchanged(t, "after prepare")
		f.runToCompletion(t, compactionOptions(nil))
		check("after rewrite")
		f.requireUnchanged(t, "after rewrite")
	})
}

// A snapshot written without the base, as an older build would after a
// rollback, is prepared again before its block is rewritten.
func TestStateCompactionRepairsSnapshotWithoutBase(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		f := newCompactionFixture(t, dbType)
		_, err := input.StateCompactionPass(ctx, f.db, compactionOptions(nil), owned)
		require.NoError(t, err)
		prepared := f.plans(t, tables.StateCompactionPrepared)
		require.NotEmpty(t, prepared)

		// Bypass the writer's safeguard and write [block, delta] directly.
		target := prepared[0]
		delta, err := f.shared.StateBlockTable.BulkInsertStateData(ctx, nil, types.StateEntries{f.renames[9]})
		require.NoError(t, err)
		var raw types.StateSnapshotNID
		require.NoError(t, f.shared.Writer.Do(f.shared.DB, nil, func(txn *sql.Tx) error {
			raw, err = f.shared.StateSnapshotTable.InsertState(ctx, txn, f.roomInfo.RoomNID, types.StateBlockNIDs{target.BlockNID, delta})
			return err
		}))
		f.want = f.states(t)
		require.NotContains(t, blockNIDs(ctx, t, f.db, raw), target.BaseNID)

		f.runToCompletion(t, compactionOptions(nil))
		require.Contains(t, blockNIDs(ctx, t, f.db, raw), target.BaseNID)
		f.expectRewritten(t)
	})
}

// A block that a snapshot written meanwhile holds after another block is
// abandoned and stays full, and a delta that equals an existing block is
// padded with an entry of the base.
func TestStateCompactionConflicts(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		f := newCompactionFixture(t, dbType)
		base := blockNIDs(ctx, t, f.db, f.full[0])[0]
		first := blockNIDs(ctx, t, f.db, f.full[1])[0]
		second := blockNIDs(ctx, t, f.db, f.full[2])[0]

		// A block that already holds the second block's delta against the
		// first block, which becomes the base once its plan is abandoned:
		// the two members renamed in the second block.
		delta := types.StateEntries{f.renames[2], f.renames[3]}
		_, err := f.shared.StateBlockTable.BulkInsertStateData(ctx, nil, slices.Clone(delta))
		require.NoError(t, err)
		f.want = f.states(t)

		// After the room is planned, a writer stores a snapshot that holds
		// the first block after the base, without the safeguard.
		var raw types.StateSnapshotNID
		hook := func(ctx context.Context, txn *sql.Tx, step string) error {
			if step != shared.StateCompactionStepPrepare || raw != 0 {
				return nil
			}
			var err error
			raw, err = f.shared.StateSnapshotTable.InsertState(ctx, txn, f.roomInfo.RoomNID, types.StateBlockNIDs{base, first})
			return err
		}
		_, err = input.StateCompactionPass(ctx, f.db, compactionOptions(hook), owned)
		require.NoError(t, err)
		require.NotZero(t, raw)
		f.want[raw] = maps.Clone(f.want[f.full[1]])
		f.requireUnchanged(t, "after the conflicting write")

		f.runToCompletion(t, compactionOptions(nil))
		byBlock := map[types.StateBlockNID]string{}
		for _, s := range []string{tables.StateCompactionAbandoned, tables.StateCompactionRewritten} {
			for _, plan := range f.plans(t, s) {
				byBlock[plan.BlockNID] = s
			}
		}
		require.Equal(t, tables.StateCompactionAbandoned, byBlock[first], "the block a snapshot holds after another one")
		require.Equal(t, tables.StateCompactionRewritten, byBlock[second], "the block whose delta equals another block")
		stored, err := f.shared.StateBlockTable.BulkSelectStateBlockEntries(ctx, nil, types.StateBlockNIDs{second})
		require.NoError(t, err)
		require.Len(t, stored[0], len(delta)+1, "the delta is padded with one entry")
	})
}

// A process that loses ownership stops before it prepares anything.
func TestStateCompactionStopsWithoutOwnership(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		f := newCompactionFixture(t, dbType)
		done, err := input.StateCompactionPass(context.Background(), f.db, compactionOptions(nil), func() bool { return false })
		require.NoError(t, err)
		require.False(t, done)
		require.Empty(t, f.plans(t, tables.StateCompactionPrepared))
		f.requireUnchanged(t, "without ownership")
	})
}

// Writers that keep writing snapshots from block lists they read long before,
// and readers that keep loading snapshots, see the state they expect while
// the compaction runs.
func TestStateCompactionUnderConcurrentWrites(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		f := newCompactionFixture(t, dbType)
		sources := slices.Sorted(maps.Keys(f.want))
		const grace = 300 * time.Millisecond

		stop := make(chan struct{})
		errs := make(chan error, 100)
		var created sync.Map // snapshot NID -> expected state
		var wg sync.WaitGroup
		for w := range 3 {
			wg.Add(2)
			go func() { // writer
				defer wg.Done()
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					source := sources[(w*7+i*13)%len(sources)]
					lists, err := f.db.StateBlockNIDs(ctx, []types.StateSnapshotNID{source})
					if err != nil {
						errs <- err
						return
					}
					// Write long after reading, past the grace period.
					time.Sleep(time.Duration((i*37+w*11)%1000) * time.Millisecond)
					member := (w*5 + i) % compactionMembers
					change := f.renames[member]
					if i%2 == 0 {
						change = f.joins[member]
					}
					nid, err := f.db.AddState(ctx, f.roomInfo.RoomNID, lists[0].StateBlockNIDs, []types.StateEntry{change})
					if err != nil {
						errs <- err
						return
					}
					want := maps.Clone(f.want[source])
					want[change.StateKeyTuple] = change.EventNID
					created.Store(nid, want)
				}
			}()
			go func() { // reader
				defer wg.Done()
				resolver := state.NewStateResolution(f.db, f.roomInfo, nil)
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					source := sources[(w*3+i*17)%len(sources)]
					entries, err := resolver.LoadStateAtSnapshot(ctx, source)
					if err != nil {
						errs <- err
						return
					}
					if !maps.Equal(f.want[source], stateMapOf(entries)) {
						errs <- fmt.Errorf("reader saw snapshot %d with a different state", source)
						return
					}
				}
			}()
		}

		opts := compactionOptions(nil)
		opts.Grace = grace
		done := false
		// Keep writing for a while after the compaction finishes too.
		deadline := time.Now().Add(6 * time.Second)
		for pass := 0; pass < 40 && (!done || time.Now().Before(deadline)); pass++ {
			if !done {
				var err error
				done, err = input.StateCompactionPass(ctx, f.db, opts, owned)
				require.NoError(t, err)
			}
			time.Sleep(grace)
		}
		close(stop)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		require.True(t, done, "the compaction finished while writers kept writing")
		f.expectRewritten(t)

		f.requireUnchanged(t, "after the concurrent run")
		got := f.states(t)
		n := 0
		created.Range(func(key, value any) bool {
			nid, ok := key.(types.StateSnapshotNID)
			require.True(t, ok)
			want, ok := value.(map[types.StateKeyTuple]types.EventNID)
			require.True(t, ok)
			require.True(t, maps.Equal(want, got[nid]), "snapshot %d written during compaction has the wrong state", nid)
			n++
			return true
		})
		require.Positive(t, n)
		t.Logf("checked %d snapshots written during compaction", n)
	})
}
