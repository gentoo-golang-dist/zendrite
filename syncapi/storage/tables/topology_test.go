package tables_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/storage/postgres"
	"codefloe.com/pat-s/zendrite/syncapi/storage/sqlite3"
	"codefloe.com/pat-s/zendrite/syncapi/storage/tables"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
)

func newTopologyTable(t *testing.T, dbType test.DBType) (tables.Topology, *sql.DB, func()) {
	t.Helper()
	connStr, close := test.PrepareDBConnectionString(t, dbType)
	db, err := sqlutil.Open(&config.DatabaseOptions{
		ConnectionString: config.DataSource(connStr),
	}, sqlutil.NewExclusiveWriter())
	if err != nil {
		t.Fatalf("failed to open db: %s", err)
	}

	var tab tables.Topology
	switch dbType {
	case test.DBTypePostgres:
		tab, err = postgres.NewPostgresTopologyTable(db)
	case test.DBTypeSQLite:
		tab, err = sqlite3.NewSqliteTopologyTable(db)
	}
	if err != nil {
		t.Fatalf("failed to make new table: %s", err)
	}
	return tab, db, close
}

func TestTopologyTable(t *testing.T) {
	ctx := context.Background()
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		tab, db, close := newTopologyTable(t, dbType)
		defer close()
		events := room.Events()
		err := sqlutil.WithTransaction(db, func(txn *sql.Tx) error {
			var highestPos types.StreamPosition
			for i, ev := range events {
				topoPos, err := tab.InsertEventInTopology(ctx, txn, ev, types.StreamPosition(i))
				if err != nil {
					return fmt.Errorf("failed to InsertEventInTopology: %w", err)
				}
				// topo pos = depth, depth starts at 1, hence 1+i
				if topoPos != types.StreamPosition(1+i) {
					return fmt.Errorf("got topo pos %d want %d", topoPos, 1+i)
				}
				highestPos = topoPos + 1
			}
			first, last := types.TopologyToken{}, types.TopologyToken{Depth: highestPos, PDUPosition: highestPos}
			// check ordering works without limit
			eventIDs, start, end, err := tab.SelectEventIDsInRange(ctx, txn, room.ID, first, last, 100, true)
			assert.NoError(t, err, "failed to SelectEventIDsInRange")
			test.AssertEventIDsEqual(t, eventIDs, events)
			assert.Equal(t, types.TopologyToken{Depth: 1, PDUPosition: 0}, start)
			assert.Equal(t, types.TopologyToken{Depth: 5, PDUPosition: 4}, end)

			eventIDs, start, end, err = tab.SelectEventIDsInRange(ctx, txn, room.ID, last, first, 100, false)
			assert.NoError(t, err, "failed to SelectEventIDsInRange")
			test.AssertEventIDsEqual(t, eventIDs, test.Reversed(events))
			assert.Equal(t, types.TopologyToken{Depth: 5, PDUPosition: 4}, start)
			assert.Equal(t, types.TopologyToken{Depth: 1, PDUPosition: 0}, end)

			// check ordering works with limit
			eventIDs, start, end, err = tab.SelectEventIDsInRange(ctx, txn, room.ID, first, last, 3, true)
			assert.NoError(t, err, "failed to SelectEventIDsInRange")
			test.AssertEventIDsEqual(t, eventIDs, events[:3])
			assert.Equal(t, types.TopologyToken{Depth: 1, PDUPosition: 0}, start)
			assert.Equal(t, types.TopologyToken{Depth: 3, PDUPosition: 2}, end)

			eventIDs, start, end, err = tab.SelectEventIDsInRange(ctx, txn, room.ID, last, first, 3, false)
			assert.NoError(t, err, "failed to SelectEventIDsInRange")
			test.AssertEventIDsEqual(t, eventIDs, test.Reversed(events[len(events)-3:]))
			assert.Equal(t, types.TopologyToken{Depth: 5, PDUPosition: 4}, start)
			assert.Equal(t, types.TopologyToken{Depth: 3, PDUPosition: 2}, end)

			// A token names the place after an event, and works in both
			// directions (MSC4492): t3_2 lies between the third and fourth
			// event, t3_1 between the second and third.
			after3, before3 := types.TopologyToken{Depth: 3, PDUPosition: 2}, types.TopologyToken{Depth: 3, PDUPosition: 1}
			eventIDs, _, _, err = tab.SelectEventIDsInRange(ctx, txn, room.ID, after3, last, 100, true)
			assert.NoError(t, err)
			test.AssertEventIDsEqual(t, eventIDs, events[3:])
			eventIDs, _, _, err = tab.SelectEventIDsInRange(ctx, txn, room.ID, after3, first, 100, false)
			assert.NoError(t, err)
			test.AssertEventIDsEqual(t, eventIDs, test.Reversed(events[:3]))
			eventIDs, _, _, err = tab.SelectEventIDsInRange(ctx, txn, room.ID, before3, last, 100, true)
			assert.NoError(t, err)
			test.AssertEventIDsEqual(t, eventIDs, events[2:])
			eventIDs, _, _, err = tab.SelectEventIDsInRange(ctx, txn, room.ID, before3, first, 100, false)
			assert.NoError(t, err)
			test.AssertEventIDsEqual(t, eventIDs, test.Reversed(events[:2]))
			// A "to" token bounds the far end the same way.
			eventIDs, _, _, err = tab.SelectEventIDsInRange(ctx, txn, room.ID, first, before3, 100, true)
			assert.NoError(t, err)
			test.AssertEventIDsEqual(t, eventIDs, events[:2])
			eventIDs, _, _, err = tab.SelectEventIDsInRange(ctx, txn, room.ID, last, before3, 100, false)
			assert.NoError(t, err)
			test.AssertEventIDsEqual(t, eventIDs, test.Reversed(events[2:]))

			// Check that we return no values for invalid rooms
			eventIDs, start, end, err = tab.SelectEventIDsInRange(ctx, txn, "!doesnotexist:localhost", last, first, 10, false)
			assert.NoError(t, err, "failed to SelectEventIDsInRange")
			assert.Equal(t, 0, len(eventIDs))
			assert.Equal(t, types.TopologyToken{}, start)
			assert.Equal(t, types.TopologyToken{}, end)
			return nil
		})
		if err != nil {
			t.Fatalf("err: %s", err)
		}
	})
}

// TestTopologyTablePagesPastForkedDepth checks backward pagination through a
// fork, where two events share a depth. A page that ends at one of them must
// continue with the other, not skip to the next depth.
func TestTopologyTablePagesPastForkedDepth(t *testing.T) {
	ctx := context.Background()
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	// Both events reference the same prev_events, so they share a depth.
	local := room.CreateEvent(t, alice, "m.room.message", map[string]any{"body": "local"})
	remote := room.CreateEvent(t, alice, "m.room.message", map[string]any{"body": "remote"})
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		tab, db, close := newTopologyTable(t, dbType)
		defer close()
		err := sqlutil.WithTransaction(db, func(txn *sql.Tx) error {
			for i, ev := range room.Events() {
				if _, err := tab.InsertEventInTopology(ctx, txn, ev, types.StreamPosition(i)); err != nil {
					return err
				}
			}
			// The remote event arrives much later, with a higher stream position.
			if _, err := tab.InsertEventInTopology(ctx, txn, local, 10); err != nil {
				return err
			}
			if _, err := tab.InsertEventInTopology(ctx, txn, remote, 20); err != nil {
				return err
			}
			top := types.TopologyToken{Depth: types.StreamPosition(local.Depth() + 1)}
			eventIDs, _, end, err := tab.SelectEventIDsInRange(ctx, txn, room.ID, top, types.TopologyToken{}, 1, false)
			if err != nil {
				return err
			}
			assert.Equal(t, []string{remote.EventID()}, eventIDs)
			end.Decrement()
			eventIDs, _, _, err = tab.SelectEventIDsInRange(ctx, txn, room.ID, end, types.TopologyToken{}, 1, false)
			if err != nil {
				return err
			}
			assert.Equal(t, []string{local.EventID()}, eventIDs, "the next page must continue at the same depth")

			// Going forward, a page that ends at the first of the two must
			// also continue with the other (MSC4492).
			start := types.TopologyToken{Depth: types.StreamPosition(local.Depth() - 1), PDUPosition: math.MaxInt64}
			eventIDs, _, end, err = tab.SelectEventIDsInRange(ctx, txn, room.ID, start, top, 1, true)
			if err != nil {
				return err
			}
			assert.Equal(t, []string{local.EventID()}, eventIDs)
			eventIDs, _, _, err = tab.SelectEventIDsInRange(ctx, txn, room.ID, end, top, 1, true)
			if err != nil {
				return err
			}
			assert.Equal(t, []string{remote.EventID()}, eventIDs, "the next page must continue at the same depth")
			return nil
		})
		if err != nil {
			t.Fatalf("err: %s", err)
		}
	})
}
