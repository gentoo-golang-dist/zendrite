// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package postgres

import (
	"context"
	"database/sql"
	"errors"

	"codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/storage/tables"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

const stateCompactionSchema = `
-- Plans of the state block compaction, which re-encodes a state block holding
-- the full state of a room as its differences from an earlier base block. The
-- row with block_nid 0 marks the compaction as finished.
CREATE TABLE IF NOT EXISTS roomserver_state_block_compaction (
	block_nid BIGINT PRIMARY KEY,
	room_nid BIGINT NOT NULL,
	base_nid BIGINT NOT NULL,
	prepared_at BIGINT NOT NULL,
	state TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS roomserver_state_block_compaction_state_idx
	ON roomserver_state_block_compaction (state);
`

const insertStateCompactionPlanSQL = "" +
	"INSERT INTO roomserver_state_block_compaction (block_nid, room_nid, base_nid, prepared_at, state)" +
	" VALUES ($1, $2, $3, $4, $5) ON CONFLICT (block_nid) DO NOTHING"

const selectStateCompactionPlansSQL = "" +
	"SELECT block_nid, room_nid, base_nid, prepared_at, state FROM roomserver_state_block_compaction" +
	" WHERE state = $1 AND block_nid <> 0 ORDER BY block_nid"

const selectRoomStateCompactionPlansSQL = "" +
	"SELECT block_nid, room_nid, base_nid, prepared_at, state FROM roomserver_state_block_compaction" +
	" WHERE room_nid = $1 AND block_nid <> 0 ORDER BY block_nid"

const lockStateCompactionPlanSQL = "" +
	"SELECT block_nid, room_nid, base_nid, prepared_at, state FROM roomserver_state_block_compaction" +
	" WHERE block_nid = $1 FOR UPDATE"

const updateStateCompactionPlanSQL = "" +
	"UPDATE roomserver_state_block_compaction SET state = $2, prepared_at = $3 WHERE block_nid = $1"

const selectStateCompactionBasesSQL = "" +
	"SELECT block_nid, base_nid FROM roomserver_state_block_compaction" +
	" WHERE block_nid = ANY($1) AND state IN ('prepared', 'rewritten')"

const selectStateCompactionDoneSQL = "" +
	"SELECT 1 FROM roomserver_state_block_compaction WHERE block_nid = 0"

const insertStateCompactionDoneSQL = "" +
	"INSERT INTO roomserver_state_block_compaction (block_nid, room_nid, base_nid, prepared_at, state)" +
	" VALUES (0, 0, 0, 0, 'done') ON CONFLICT (block_nid) DO NOTHING"

const selectStateCompactionRoomNIDsSQL = "" +
	"SELECT DISTINCT room_nid FROM roomserver_state_snapshots ORDER BY room_nid"

const selectStateCompactionRoomSnapshotsSQL = "" +
	"SELECT state_snapshot_nid, state_block_nids FROM roomserver_state_snapshots" +
	" WHERE room_nid = $1 ORDER BY state_snapshot_nid"

const selectStateCompactionSnapshotsWithBlockSQL = "" +
	"SELECT state_snapshot_nid, state_block_nids FROM roomserver_state_snapshots" +
	" WHERE room_nid = $1 AND $2 = ANY(state_block_nids) ORDER BY state_snapshot_nid"

const updateStateCompactionSnapshotSQL = "" +
	"UPDATE roomserver_state_snapshots SET state_block_nids = $3, state_snapshot_hash = $4" +
	" WHERE state_snapshot_nid = $1 AND state_snapshot_hash = $2"

const selectStateCompactionSnapshotExistsSQL = "" +
	"SELECT 1 FROM roomserver_state_snapshots WHERE state_snapshot_hash = $1"

const updateStateCompactionBlockSQL = "" +
	"UPDATE roomserver_state_block SET event_nids = $3, state_block_hash = $4" +
	" WHERE state_block_nid = $1 AND state_block_hash = $2"

const selectStateCompactionBlockExistsSQL = "" +
	"SELECT 1 FROM roomserver_state_block WHERE state_block_hash = $1"

// Only client backends write the room server's tables; background workers
// such as autovacuum also report transactions.
const selectOldestTransactionStartSQL = "" +
	"SELECT (EXTRACT(EPOCH FROM MIN(xact_start)) * 1000)::bigint FROM pg_stat_activity" +
	" WHERE datname = current_database() AND pid <> pg_backend_pid()" +
	" AND backend_type = 'client backend' AND xact_start IS NOT NULL"

type stateCompactionStatements struct {
	insertPlanStmt               *sql.Stmt
	selectPlansStmt              *sql.Stmt
	selectRoomPlansStmt          *sql.Stmt
	lockPlanStmt                 *sql.Stmt
	updatePlanStmt               *sql.Stmt
	selectBasesStmt              *sql.Stmt
	selectDoneStmt               *sql.Stmt
	insertDoneStmt               *sql.Stmt
	selectRoomNIDsStmt           *sql.Stmt
	selectRoomSnapshotsStmt      *sql.Stmt
	selectSnapshotsWithBlockStmt *sql.Stmt
	updateSnapshotStmt           *sql.Stmt
	selectSnapshotExistsStmt     *sql.Stmt
	updateBlockStmt              *sql.Stmt
	selectBlockExistsStmt        *sql.Stmt
	selectOldestTransactionStmt  *sql.Stmt
}

func CreateStateCompactionTable(db *sql.DB) error {
	_, err := db.Exec(stateCompactionSchema)
	return err
}

func PrepareStateCompactionTable(db *sql.DB) (tables.StateCompaction, error) {
	s := &stateCompactionStatements{}
	return s, sqlutil.StatementList{
		{&s.insertPlanStmt, insertStateCompactionPlanSQL},
		{&s.selectPlansStmt, selectStateCompactionPlansSQL},
		{&s.selectRoomPlansStmt, selectRoomStateCompactionPlansSQL},
		{&s.lockPlanStmt, lockStateCompactionPlanSQL},
		{&s.updatePlanStmt, updateStateCompactionPlanSQL},
		{&s.selectBasesStmt, selectStateCompactionBasesSQL},
		{&s.selectDoneStmt, selectStateCompactionDoneSQL},
		{&s.insertDoneStmt, insertStateCompactionDoneSQL},
		{&s.selectRoomNIDsStmt, selectStateCompactionRoomNIDsSQL},
		{&s.selectRoomSnapshotsStmt, selectStateCompactionRoomSnapshotsSQL},
		{&s.selectSnapshotsWithBlockStmt, selectStateCompactionSnapshotsWithBlockSQL},
		{&s.updateSnapshotStmt, updateStateCompactionSnapshotSQL},
		{&s.selectSnapshotExistsStmt, selectStateCompactionSnapshotExistsSQL},
		{&s.updateBlockStmt, updateStateCompactionBlockSQL},
		{&s.selectBlockExistsStmt, selectStateCompactionBlockExistsSQL},
		{&s.selectOldestTransactionStmt, selectOldestTransactionStartSQL},
	}.Prepare(db)
}

func (s *stateCompactionStatements) InsertPlan(ctx context.Context, txn *sql.Tx, plan tables.StateCompactionPlan) (bool, error) {
	res, err := sqlutil.TxStmt(txn, s.insertPlanStmt).ExecContext(ctx,
		int64(plan.BlockNID), int64(plan.RoomNID), int64(plan.BaseNID), plan.PreparedAt, plan.State)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func scanStateCompactionPlans(ctx context.Context, rows *sql.Rows, err error) ([]tables.StateCompactionPlan, error) {
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "scanStateCompactionPlans: rows.close() failed")
	var plans []tables.StateCompactionPlan
	for rows.Next() {
		var plan tables.StateCompactionPlan
		if err = rows.Scan(&plan.BlockNID, &plan.RoomNID, &plan.BaseNID, &plan.PreparedAt, &plan.State); err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

func (s *stateCompactionStatements) SelectPlans(ctx context.Context, txn *sql.Tx, state string) ([]tables.StateCompactionPlan, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectPlansStmt).QueryContext(ctx, state)
	return scanStateCompactionPlans(ctx, rows, err)
}

func (s *stateCompactionStatements) SelectRoomPlans(ctx context.Context, txn *sql.Tx, roomNID types.RoomNID) ([]tables.StateCompactionPlan, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectRoomPlansStmt).QueryContext(ctx, int64(roomNID))
	return scanStateCompactionPlans(ctx, rows, err)
}

func (s *stateCompactionStatements) LockPlan(ctx context.Context, txn *sql.Tx, blockNID types.StateBlockNID) (*tables.StateCompactionPlan, error) {
	var plan tables.StateCompactionPlan
	err := sqlutil.TxStmt(txn, s.lockPlanStmt).QueryRowContext(ctx, int64(blockNID)).
		Scan(&plan.BlockNID, &plan.RoomNID, &plan.BaseNID, &plan.PreparedAt, &plan.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

func (s *stateCompactionStatements) UpdatePlan(ctx context.Context, txn *sql.Tx, blockNID types.StateBlockNID, state string, preparedAt int64) error {
	_, err := sqlutil.TxStmt(txn, s.updatePlanStmt).ExecContext(ctx, int64(blockNID), state, preparedAt)
	return err
}

func (s *stateCompactionStatements) SelectBases(
	ctx context.Context, txn *sql.Tx, blockNIDs []types.StateBlockNID,
) (map[types.StateBlockNID]types.StateBlockNID, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectBasesStmt).QueryContext(ctx, stateBlockNIDsAsArray(blockNIDs))
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectBases: rows.close() failed")
	bases := map[types.StateBlockNID]types.StateBlockNID{}
	for rows.Next() {
		var block, base types.StateBlockNID
		if err = rows.Scan(&block, &base); err != nil {
			return nil, err
		}
		bases[block] = base
	}
	return bases, rows.Err()
}

func (s *stateCompactionStatements) SelectDone(ctx context.Context, txn *sql.Tx) (bool, error) {
	var one int
	err := sqlutil.TxStmt(txn, s.selectDoneStmt).QueryRowContext(ctx).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *stateCompactionStatements) InsertDone(ctx context.Context, txn *sql.Tx) error {
	_, err := sqlutil.TxStmt(txn, s.insertDoneStmt).ExecContext(ctx)
	return err
}

func (s *stateCompactionStatements) SelectRoomNIDs(ctx context.Context, txn *sql.Tx) ([]types.RoomNID, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectRoomNIDsStmt).QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "SelectRoomNIDs: rows.close() failed")
	var roomNIDs []types.RoomNID
	for rows.Next() {
		var roomNID types.RoomNID
		if err = rows.Scan(&roomNID); err != nil {
			return nil, err
		}
		roomNIDs = append(roomNIDs, roomNID)
	}
	return roomNIDs, rows.Err()
}

func scanStateCompactionSnapshots(ctx context.Context, rows *sql.Rows, err error) ([]types.StateBlockNIDList, error) {
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "scanStateCompactionSnapshots: rows.close() failed")
	var lists []types.StateBlockNIDList
	for rows.Next() {
		var list types.StateBlockNIDList
		var blocks sqlutil.Int64Array
		if err = rows.Scan(&list.StateSnapshotNID, &blocks); err != nil {
			return nil, err
		}
		list.StateBlockNIDs = make([]types.StateBlockNID, len(blocks))
		for i, block := range blocks {
			list.StateBlockNIDs[i] = types.StateBlockNID(block)
		}
		lists = append(lists, list)
	}
	return lists, rows.Err()
}

func (s *stateCompactionStatements) SelectRoomSnapshots(ctx context.Context, txn *sql.Tx, roomNID types.RoomNID) ([]types.StateBlockNIDList, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectRoomSnapshotsStmt).QueryContext(ctx, int64(roomNID))
	return scanStateCompactionSnapshots(ctx, rows, err)
}

func (s *stateCompactionStatements) SelectSnapshotsWithBlock(
	ctx context.Context, txn *sql.Tx, roomNID types.RoomNID, blockNID types.StateBlockNID,
) ([]types.StateBlockNIDList, error) {
	rows, err := sqlutil.TxStmt(txn, s.selectSnapshotsWithBlockStmt).QueryContext(ctx, int64(roomNID), int64(blockNID))
	return scanStateCompactionSnapshots(ctx, rows, err)
}

func (s *stateCompactionStatements) UpdateSnapshotBlocks(
	ctx context.Context, txn *sql.Tx, snapshotNID types.StateSnapshotNID, oldBlocks, newBlocks types.StateBlockNIDs,
) (bool, error) {
	res, err := sqlutil.TxStmt(txn, s.updateSnapshotStmt).ExecContext(ctx,
		int64(snapshotNID), oldBlocks.Hash(), stateBlockNIDsAsArray(newBlocks), newBlocks.Hash())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *stateCompactionStatements) SnapshotExists(ctx context.Context, txn *sql.Tx, blocks types.StateBlockNIDs) (bool, error) {
	return exists(ctx, sqlutil.TxStmt(txn, s.selectSnapshotExistsStmt), blocks.Hash())
}

func (s *stateCompactionStatements) UpdateBlockEvents(
	ctx context.Context, txn *sql.Tx, blockNID types.StateBlockNID, oldEvents, newEvents types.EventNIDs,
) (bool, error) {
	res, err := sqlutil.TxStmt(txn, s.updateBlockStmt).ExecContext(ctx,
		int64(blockNID), oldEvents.Hash(), eventNIDsAsArray(newEvents), newEvents.Hash())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *stateCompactionStatements) BlockExists(ctx context.Context, txn *sql.Tx, events types.EventNIDs) (bool, error) {
	return exists(ctx, sqlutil.TxStmt(txn, s.selectBlockExistsStmt), events.Hash())
}

func (s *stateCompactionStatements) SelectOldestTransactionStart(ctx context.Context, txn *sql.Tx) (int64, bool, error) {
	var start sql.NullInt64
	if err := sqlutil.TxStmt(txn, s.selectOldestTransactionStmt).QueryRowContext(ctx).Scan(&start); err != nil {
		return 0, false, err
	}
	return start.Int64, start.Valid, nil
}

func exists(ctx context.Context, stmt *sql.Stmt, args ...any) (bool, error) {
	var one int
	err := stmt.QueryRowContext(ctx, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
