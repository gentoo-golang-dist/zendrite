// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package storage_test

import (
	"context"
	"database/sql"
	"math"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	postgresdeltas "codefloe.com/pat-s/zendrite/syncapi/storage/postgres/deltas"
	sqlitedeltas "codefloe.com/pat-s/zendrite/syncapi/storage/sqlite3/deltas"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
)

// relationLabelRoom is a room whose messages have different relations
// (MSC3874) and labels (MSC2326).
type relationLabelRoom struct {
	room                                     *test.Room
	root, thread, reference, plain, oddLabel *rstypes.HeaderedEvent
	messages                                 []*rstypes.HeaderedEvent
}

func newRelationLabelRoom(t *testing.T, alice *test.User) relationLabelRoom {
	t.Helper()
	r := relationLabelRoom{room: test.NewRoom(t, alice)}
	r.root = r.room.CreateAndInsert(t, alice, "m.room.message", map[string]any{
		"body": "root", "org.matrix.labels": []string{"#fun", "#work"},
	})
	r.thread = r.room.CreateAndInsert(t, alice, "m.room.message", map[string]any{
		"body":         "thread reply",
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": r.root.EventID()},
	})
	r.reference = r.room.CreateAndInsert(t, alice, "m.room.message", map[string]any{
		"body":              "reference reply",
		"m.relates_to":      map[string]any{"rel_type": "m.reference", "event_id": r.root.EventID()},
		"org.matrix.labels": []string{"#fun"},
	})
	// A labels field that is not an array gives no labels.
	r.plain = r.room.CreateAndInsert(t, alice, "m.room.message", map[string]any{
		"body": "plain", "org.matrix.labels": "#fun",
	})
	// Only the strings of the array are labels.
	r.oddLabel = r.room.CreateAndInsert(t, alice, "m.room.message", map[string]any{
		"body": "odd label", "org.matrix.labels": []any{1, "#work"},
	})
	r.messages = []*rstypes.HeaderedEvent{r.root, r.thread, r.reference, r.plain, r.oddLabel}
	return r
}

type relationLabelCase struct {
	name   string
	filter synctypes.RoomEventFilter
	want   []*rstypes.HeaderedEvent
}

func relationLabelCases(r relationLabelRoom) []relationLabelCase {
	list := func(values ...string) *[]string { return &values }
	return []relationLabelCase{
		{"rel_types thread", synctypes.RoomEventFilter{RelTypes: list("m.thread")}, []*rstypes.HeaderedEvent{r.thread}},
		{"rel_types reference", synctypes.RoomEventFilter{RelTypes: list("m.reference")}, []*rstypes.HeaderedEvent{r.reference}},
		{"rel_types both", synctypes.RoomEventFilter{RelTypes: list("m.thread", "m.reference")}, []*rstypes.HeaderedEvent{r.thread, r.reference}},
		{"rel_types empty", synctypes.RoomEventFilter{RelTypes: list()}, nil},
		{"not_rel_types thread", synctypes.RoomEventFilter{NotRelTypes: list("m.thread")}, []*rstypes.HeaderedEvent{r.root, r.reference, r.plain, r.oddLabel}},
		{"not_rel_types both", synctypes.RoomEventFilter{NotRelTypes: list("m.thread", "m.reference")}, []*rstypes.HeaderedEvent{r.root, r.plain, r.oddLabel}},
		{"not_rel_types empty", synctypes.RoomEventFilter{NotRelTypes: list()}, r.messages},
		{"not_rel_types wins", synctypes.RoomEventFilter{RelTypes: list("m.thread", "m.reference"), NotRelTypes: list("m.thread")}, []*rstypes.HeaderedEvent{r.reference}},
		{"labels fun", synctypes.RoomEventFilter{Labels: list("#fun")}, []*rstypes.HeaderedEvent{r.root, r.reference}},
		{"labels work", synctypes.RoomEventFilter{Labels: list("#work")}, []*rstypes.HeaderedEvent{r.root, r.oddLabel}},
		{"labels empty", synctypes.RoomEventFilter{Labels: list()}, nil},
		{"not_labels fun", synctypes.RoomEventFilter{NotLabels: list("#fun")}, []*rstypes.HeaderedEvent{r.thread, r.plain, r.oddLabel}},
		{"labels and not_labels", synctypes.RoomEventFilter{Labels: list("#work"), NotLabels: list("#fun")}, []*rstypes.HeaderedEvent{r.oddLabel}},
		{"labels and rel_types", synctypes.RoomEventFilter{Labels: list("#fun"), RelTypes: list("m.reference")}, []*rstypes.HeaderedEvent{r.reference}},
		{"no relation or label filter", synctypes.RoomEventFilter{}, r.messages},
	}
}

func eventIDs(events []*rstypes.HeaderedEvent) []string {
	ids := make([]string, 0, len(events))
	for _, ev := range events {
		ids = append(ids, ev.EventID())
	}
	return ids
}

func streamEventIDs(events []types.StreamEvent) []string {
	ids := make([]string, 0, len(events))
	for _, ev := range events {
		ids = append(ids, ev.EventID())
	}
	return ids
}

// The relation type (MSC3874) and label (MSC2326) filters apply to /messages,
// to sync timelines in stream and topological order, and to /context.
func TestRelationAndLabelFilters(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := MustCreateDatabase(t, dbType)
		defer closeDB()
		alice := test.NewUser(t)
		r := newRelationLabelRoom(t, alice)
		positions := MustWriteEvents(t, db, r.room.Events())
		// Another room's events must not leak into the results.
		MustWriteEvents(t, db, newRelationLabelRoom(t, alice).room.Events())
		firstMessagePos := positions[len(positions)-len(r.messages)]
		// Read once: on SQLite, SelectMaxEventID closes its shared statement.
		var max types.StreamPosition
		withRolledBackSnapshot(t, db, func(snapshot storage.DatabaseTransaction) {
			var err error
			max, err = snapshot.MaxStreamPositionForPDUs(ctx)
			require.NoError(t, err)
		})

		for _, tc := range relationLabelCases(r) {
			t.Run(tc.name, func(t *testing.T) {
				want := eventIDs(tc.want)
				messageTypes := []string{"m.room.message"}
				withRolledBackSnapshot(t, db, func(snapshot storage.DatabaseTransaction) {
					filter := tc.filter
					filter.Limit = 100
					filter.Types = &messageTypes

					// /messages
					from := types.TopologyToken{}
					to := types.TopologyToken{Depth: math.MaxInt64, PDUPosition: math.MaxInt64}
					f := filter
					events, _, _, err := snapshot.GetEventsInTopologicalRange(ctx, &from, &to, r.room.ID, &f, false)
					require.NoError(t, err)
					require.ElementsMatch(t, want, streamEventIDs(events), "/messages")

					// /sync in stream order.
					f = filter
					recent, err := snapshot.RecentEvents(ctx, []string{r.room.ID}, types.Range{From: 0, To: max}, &f, true, true)
					require.NoError(t, err)
					require.ElementsMatch(t, want, streamEventIDs(recent[r.room.ID].Events), "/sync")

					// /sync in topological order (MSC4445).
					f = filter
					byTopology, err := snapshot.RecentEventsByTopology(ctx, []string{r.room.ID}, max, &f)
					require.NoError(t, err)
					require.ElementsMatch(t, want, streamEventIDs(byTopology[r.room.ID].Events), "/sync by topology")

					// /context around the room's creation event and after
					// its last event.
					f = filter
					_, after, err := snapshot.SelectContextAfterEvent(ctx, int(firstMessagePos)-1, r.room.ID, &f)
					require.NoError(t, err)
					require.ElementsMatch(t, want, eventIDs(after), "/context after")
					f = filter
					before, err := snapshot.SelectContextBeforeEvent(ctx, int(positions[len(positions)-1])+1, r.room.ID, &f)
					require.NoError(t, err)
					require.ElementsMatch(t, want, eventIDs(before), "/context before")
				})
			})
		}
	})
}

// A redacted event loses its labels and its relation.
func TestRelationAndLabelFiltersAfterRedaction(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := MustCreateDatabase(t, dbType)
		defer closeDB()
		alice := test.NewUser(t)
		r := newRelationLabelRoom(t, alice)
		MustWriteEvents(t, db, r.room.Events())

		for _, ev := range []*rstypes.HeaderedEvent{r.root, r.reference} {
			redaction := r.room.CreateEvent(t, alice, spec.MRoomRedaction, map[string]string{"redacts": ev.EventID()})
			require.NoError(t, db.RedactEvent(ctx, ev.EventID(), redaction, &FakeQuerier{}))
			require.NoError(t, db.RedactRelations(ctx, r.room.ID, ev.EventID()))
		}

		list := func(values ...string) *[]string { return &values }
		withRolledBackSnapshot(t, db, func(snapshot storage.DatabaseTransaction) {
			from := types.TopologyToken{}
			to := types.TopologyToken{Depth: math.MaxInt64, PDUPosition: math.MaxInt64}
			events, _, _, err := snapshot.GetEventsInTopologicalRange(ctx, &from, &to, r.room.ID, &synctypes.RoomEventFilter{Limit: 100, Labels: list("#fun")}, false)
			require.NoError(t, err)
			require.Empty(t, streamEventIDs(events))
			events, _, _, err = snapshot.GetEventsInTopologicalRange(ctx, &from, &to, r.room.ID, &synctypes.RoomEventFilter{Limit: 100, RelTypes: list("m.reference", "m.thread")}, false)
			require.NoError(t, err)
			require.Equal(t, []string{r.thread.EventID()}, streamEventIDs(events))
		})
	})
}

// The migration stores the labels of the events stored before the labels
// table existed.
func TestEventLabelsMigration(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		alice := test.NewUser(t)
		r := newRelationLabelRoom(t, alice)

		connStr, closeDB := test.PrepareDBConnectionString(t, dbType)
		t.Cleanup(closeDB)
		db, err := storage.NewSyncServerDatasource(ctx, sqlutil.NewConnectionManager(nil, config.DatabaseOptions{}), &config.DatabaseOptions{
			ConnectionString: config.DataSource(connStr),
		})
		require.NoError(t, err)
		MustWriteEvents(t, db, r.room.Events())

		driver, up := "pgx", postgresdeltas.UpEventLabels
		if dbType == test.DBTypeSQLite {
			driver, up = sqlutil.SQLITE_DRIVER_NAME, sqlitedeltas.UpEventLabels
		}
		raw, err := sql.Open(driver, connStr)
		require.NoError(t, err)
		t.Cleanup(func() { _ = raw.Close() })
		_, err = raw.Exec(`DELETE FROM syncapi_event_labels`)
		require.NoError(t, err)

		// Twice, as a migration that was interrupted after committing runs
		// again.
		for range 2 {
			tx, err := raw.BeginTx(ctx, nil)
			require.NoError(t, err)
			require.NoError(t, up(ctx, tx))
			require.NoError(t, tx.Commit())
		}

		rows, err := raw.Query(`SELECT event_id, label FROM syncapi_event_labels ORDER BY event_id, label`)
		require.NoError(t, err)
		defer rows.Close()
		got := map[string][]string{}
		for rows.Next() {
			var eventID, label string
			require.NoError(t, rows.Scan(&eventID, &label))
			got[eventID] = append(got[eventID], label)
		}
		require.NoError(t, rows.Err())
		require.Equal(t, map[string][]string{
			r.root.EventID():      {"#fun", "#work"},
			r.reference.EventID(): {"#fun"},
			r.oddLabel.EventID():  {"#work"},
		}, got)
	})
}

// withRolledBackSnapshot is WithSnapshot, but also rolls the snapshot back
// when f fails the test, so that the database can be cleaned up.
func withRolledBackSnapshot(t *testing.T, db storage.Database, f func(snapshot storage.DatabaseTransaction)) {
	t.Helper()
	snapshot, err := db.NewDatabaseSnapshot(ctx)
	require.NoError(t, err)
	defer snapshot.Rollback() //nolint:errcheck
	f(snapshot)
}
