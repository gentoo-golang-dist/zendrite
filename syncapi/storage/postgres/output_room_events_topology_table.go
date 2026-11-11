// Copyright 2018-2024 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"

	"codefloe.com/pat-s/gomatrixserverlib"

	"codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/internal/depth"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/syncapi/storage/tables"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

const outputRoomEventsTopologySchema = `
-- Stores output room events received from the roomserver.
CREATE TABLE IF NOT EXISTS syncapi_output_room_events_topology (
	-- The event ID for the event.
    event_id TEXT PRIMARY KEY,
	-- The place of the event in the room's topology. This can usually be determined
	-- from the event's depth.
	topological_position BIGINT NOT NULL,
	stream_position BIGINT NOT NULL,
    -- The 'room_id' key for the event.
    room_id TEXT NOT NULL
);
-- The topological order will be used in events selection and ordering
CREATE UNIQUE INDEX IF NOT EXISTS syncapi_event_topological_position_idx ON syncapi_output_room_events_topology(topological_position, stream_position, room_id);
-- The latest events of a room in topological order, for the timelines of
-- rooms a sync sends for the first time (MSC4445).
CREATE INDEX IF NOT EXISTS syncapi_event_topology_room_idx ON syncapi_output_room_events_topology(room_id, topological_position, stream_position);
`

const insertEventInTopologySQL = "" +
	"INSERT INTO syncapi_output_room_events_topology (event_id, topological_position, room_id, stream_position)" +
	" VALUES ($1, $2, $3, $4)" +
	" ON CONFLICT (topological_position, stream_position, room_id) DO UPDATE SET event_id = $1" +
	" RETURNING topological_position"

const selectEventIDsInRangeASCSQL = "" +
	"SELECT event_id, topological_position, stream_position FROM syncapi_output_room_events_topology" +
	" WHERE room_id = $1 AND (" +
	"(topological_position > $2 AND topological_position < $3) OR" +
	"(topological_position = $4 AND stream_position >= $5)" +
	") ORDER BY topological_position ASC, stream_position ASC LIMIT $6"

const selectEventIDsInRangeDESCSQL = "" +
	"SELECT event_id, topological_position, stream_position FROM syncapi_output_room_events_topology" +
	" WHERE room_id = $1 AND (" +
	"(topological_position > $2 AND topological_position < $3) OR" +
	"(topological_position = $4 AND stream_position <= $5)" +
	") ORDER BY topological_position DESC, stream_position DESC LIMIT $6"

const selectEventIDsBeforeStreamSQL = "" +
	"SELECT t.event_id, t.topological_position, t.stream_position FROM syncapi_output_room_events_topology t" +
	" JOIN syncapi_output_room_events e ON e.event_id = t.event_id" +
	" WHERE t.room_id = $1 AND t.topological_position > $2" +
	" AND (t.stream_position <= $3 OR e.exclude_from_sync)" +
	" ORDER BY t.topological_position DESC, t.stream_position DESC LIMIT $4"

// selectRecentEventsByTopologySQL selects the latest events of each room in
// the order of the room's DAG (MSC4445): by depth, with the stream position
// breaking ties, among the events the stream delivered up to $2. It reads
// the topology table through its (room_id, topological_position,
// stream_position) index, latest first.
var selectRecentEventsByTopologySQL = `
WITH room_ids AS (
     SELECT unnest($1::text[]) AS room_id
)
SELECT    x.*
FROM room_ids,
          LATERAL  (
              SELECT e.room_id, e.event_id, e.id, e.headered_event_json, e.session_id, e.exclude_from_sync, e.transaction_id, e.history_visibility, t.topological_position
                    FROM syncapi_output_room_events_topology t
                    JOIN syncapi_output_room_events e ON e.event_id = t.event_id
                    WHERE
                      t.room_id = room_ids.room_id
                      AND t.stream_position <= $2
                      AND e.exclude_from_sync = FALSE
                      AND ( $3::text[] IS NULL OR     e.sender  = ANY($3)  )
                      AND ( $4::text[] IS NULL OR NOT(e.sender  = ANY($4)) )
                      AND ( $5::text[] IS NULL OR     e.type LIKE ANY($5)  )
                      AND ( $6::text[] IS NULL OR NOT(e.type LIKE ANY($6)) )
                      ` + relationLabelFilterSQL("e.event_id", 8) + //nolint:mnd // the parameters after the others
	`
                    ORDER BY t.topological_position DESC, t.stream_position DESC
                    LIMIT $7
              ) AS x
`

const selectPositionInTopologySQL = "" +
	"SELECT topological_position, stream_position FROM syncapi_output_room_events_topology" +
	" WHERE event_id = $1"

const selectStreamToTopologicalPositionAscSQL = "" +
	"SELECT topological_position FROM syncapi_output_room_events_topology WHERE room_id = $1 AND stream_position >= $2 ORDER BY topological_position ASC LIMIT 1;"

const selectStreamToTopologicalPositionDescSQL = "" +
	"SELECT topological_position FROM syncapi_output_room_events_topology WHERE room_id = $1 AND stream_position <= $2 ORDER BY topological_position DESC LIMIT 1;"

const deleteEventTopologySQL = "" +
	"DELETE FROM syncapi_output_room_events_topology WHERE event_id = $1"

const purgeEventsTopologySQL = "" +
	"DELETE FROM syncapi_output_room_events_topology WHERE room_id = $1"

type outputRoomEventsTopologyStatements struct {
	db                                        *sql.DB
	insertEventInTopologyStmt                 *sql.Stmt
	selectEventIDsInRangeASCStmt              *sql.Stmt
	selectEventIDsInRangeDESCStmt             *sql.Stmt
	selectPositionInTopologyStmt              *sql.Stmt
	selectStreamToTopologicalPositionAscStmt  *sql.Stmt
	selectStreamToTopologicalPositionDescStmt *sql.Stmt
	purgeEventsTopologyStmt                   *sql.Stmt
	deleteEventTopologyStmt                   *sql.Stmt
}

func NewPostgresTopologyTable(db *sql.DB) (tables.Topology, error) {
	s := &outputRoomEventsTopologyStatements{db: db}
	_, err := db.Exec(outputRoomEventsTopologySchema)
	if err != nil {
		return nil, err
	}
	return s, sqlutil.StatementList{
		{&s.insertEventInTopologyStmt, insertEventInTopologySQL},
		{&s.selectEventIDsInRangeASCStmt, selectEventIDsInRangeASCSQL},
		{&s.selectEventIDsInRangeDESCStmt, selectEventIDsInRangeDESCSQL},
		{&s.selectPositionInTopologyStmt, selectPositionInTopologySQL},
		{&s.selectStreamToTopologicalPositionAscStmt, selectStreamToTopologicalPositionAscSQL},
		{&s.selectStreamToTopologicalPositionDescStmt, selectStreamToTopologicalPositionDescSQL},
		{&s.purgeEventsTopologyStmt, purgeEventsTopologySQL},
		{&s.deleteEventTopologyStmt, deleteEventTopologySQL},
	}.Prepare(db)
}

// InsertEventInTopology inserts the given event in the room's topology, based
// on the event's depth.
func (s *outputRoomEventsTopologyStatements) InsertEventInTopology(
	ctx context.Context, txn *sql.Tx, event *rstypes.HeaderedEvent, pos types.StreamPosition,
) (topoPos types.StreamPosition, err error) {
	// Clamp the depth to prevent issues with events that have depth values
	// exceeding the canonical JSON integer limit (e.g., from corrupt federation data).
	depth := depth.Clamp(event.Depth())
	err = sqlutil.TxStmt(txn, s.insertEventInTopologyStmt).QueryRowContext(
		ctx, event.EventID(), depth, event.RoomID().String(), pos,
	).Scan(&topoPos)
	return
}

// SelectEventIDsInRange selects the IDs of events which positions are within a
// given range in a given room's topological order. Returns the start/end topological tokens for
// the returned eventIDs.
// Returns an empty slice if no events match the given range.
func (s *outputRoomEventsTopologyStatements) SelectEventIDsInRange(
	ctx context.Context, txn *sql.Tx, roomID string, minDepth, maxDepth, maxStreamPos types.StreamPosition,
	limit int, chronologicalOrder bool,
) (eventIDs []string, start, end types.TopologyToken, err error) {
	// Decide on the selection's order according to whether chronological order
	// is requested or not.
	var stmt *sql.Stmt
	if chronologicalOrder {
		stmt = sqlutil.TxStmt(txn, s.selectEventIDsInRangeASCStmt)
	} else {
		stmt = sqlutil.TxStmt(txn, s.selectEventIDsInRangeDESCStmt)
	}

	// Query the event IDs.
	rows, err := stmt.QueryContext(ctx, roomID, minDepth, maxDepth, maxDepth, maxStreamPos, limit)
	if errors.Is(err, sql.ErrNoRows) {
		// If no event matched the request, return an empty slice.
		return []string{}, start, end, nil
	} else if err != nil {
		return
	}
	defer internal.CloseAndLogIfError(ctx, rows, "selectEventIDsInRange: rows.close() failed")

	// Return the IDs.
	var eventID string
	var token types.TopologyToken
	var tokens []types.TopologyToken
	for rows.Next() {
		if err = rows.Scan(&eventID, &token.Depth, &token.PDUPosition); err != nil {
			return
		}
		eventIDs = append(eventIDs, eventID)
		tokens = append(tokens, token)
	}

	// The values are already ordered by SQL, so we can use them as is.
	if len(tokens) > 0 {
		start = tokens[0]
		end = tokens[len(tokens)-1]
	}

	return eventIDs, start, end, rows.Err()
}

// SelectEventIDsBeforeStream selects the events of a room that the stream
// delivered up to streamBound or that never came down it, latest depth first.
func (s *outputRoomEventsTopologyStatements) SelectEventIDsBeforeStream(
	ctx context.Context, txn *sql.Tx, roomID string, minDepth, streamBound types.StreamPosition, limit int,
) (eventIDs []string, start, end types.TopologyToken, err error) {
	// Not prepared with the others, as it reads a table that may not exist
	// yet when this one is set up.
	var rows *sql.Rows
	if txn != nil {
		rows, err = txn.QueryContext(ctx, selectEventIDsBeforeStreamSQL, roomID, minDepth, streamBound, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, selectEventIDsBeforeStreamSQL, roomID, minDepth, streamBound, limit)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return []string{}, start, end, nil
	} else if err != nil {
		return
	}
	defer internal.CloseAndLogIfError(ctx, rows, "selectEventIDsBeforeStream: rows.close() failed")

	var eventID string
	var token types.TopologyToken
	for rows.Next() {
		if err = rows.Scan(&eventID, &token.Depth, &token.PDUPosition); err != nil {
			return
		}
		if len(eventIDs) == 0 {
			start = token
		}
		eventIDs = append(eventIDs, eventID)
		end = token
	}
	return eventIDs, start, end, rows.Err()
}

// SelectPositionInTopology returns the position of a given event in the
// topology of the room it belongs to.
func (s *outputRoomEventsTopologyStatements) SelectPositionInTopology(
	ctx context.Context, txn *sql.Tx, eventID string,
) (pos, spos types.StreamPosition, err error) {
	err = sqlutil.TxStmt(txn, s.selectPositionInTopologyStmt).QueryRowContext(ctx, eventID).Scan(&pos, &spos)
	return
}

// SelectStreamToTopologicalPosition returns the closest position of a given event
// in the topology of the room it belongs to from the given stream position.
func (s *outputRoomEventsTopologyStatements) SelectStreamToTopologicalPosition(
	ctx context.Context, txn *sql.Tx, roomID string, streamPos types.StreamPosition, backwardOrdering bool,
) (topoPos types.StreamPosition, err error) {
	if backwardOrdering {
		err = sqlutil.TxStmt(txn, s.selectStreamToTopologicalPositionDescStmt).QueryRowContext(ctx, roomID, streamPos).Scan(&topoPos)
	} else {
		err = sqlutil.TxStmt(txn, s.selectStreamToTopologicalPositionAscStmt).QueryRowContext(ctx, roomID, streamPos).Scan(&topoPos)
	}
	return
}

// DeleteEventTopology removes an event from the topology.
func (s *outputRoomEventsTopologyStatements) DeleteEventTopology(ctx context.Context, txn *sql.Tx, eventID string) error {
	_, err := sqlutil.TxStmt(txn, s.deleteEventTopologyStmt).ExecContext(ctx, eventID)
	return err
}

func (s *outputRoomEventsTopologyStatements) PurgeEventsTopology(
	ctx context.Context, txn *sql.Tx, roomID string,
) error {
	purgeEventsTopologyStmt := sqlutil.TxStmt(txn, s.purgeEventsTopologyStmt)
	_, err := purgeEventsTopologyStmt.ExecContext(ctx, roomID)
	return err
}

// SelectRecentEventsByTopology returns, for each room, the latest `limit`
// events up to the stream position `to` in topological order, oldest first.
// Events kept from sync are left out. The result is limited when the room
// holds more such events.
func (s *outputRoomEventsTopologyStatements) SelectRecentEventsByTopology(
	ctx context.Context, txn *sql.Tx,
	roomIDs []string, to types.StreamPosition, eventFilter *synctypes.RoomEventFilter,
) (map[string]types.RecentEvents, error) {
	senders, notSenders := getSendersRoomEventFilter(eventFilter)
	params := withRelationLabelArgs(eventFilter,
		roomIDs, to,
		senders,
		notSenders,
		filterConvertTypeWildcardToSQL(eventFilter.Types),
		filterConvertTypeWildcardToSQL(eventFilter.NotTypes),
		eventFilter.Limit+1,
	)
	// Not prepared with the others, as it reads a table that may not exist
	// yet when this one is set up.
	var rows *sql.Rows
	var err error
	if txn != nil {
		rows, err = txn.QueryContext(ctx, selectRecentEventsByTopologySQL, params...)
	} else {
		rows, err = s.db.QueryContext(ctx, selectRecentEventsByTopologySQL, params...)
	}
	if err != nil {
		return nil, err
	}
	defer internal.CloseAndLogIfError(ctx, rows, "selectRecentEventsByTopology: rows.close() failed")

	type topoEvent struct {
		types.StreamEvent
		topoPos types.StreamPosition
	}
	byRoom := make(map[string][]topoEvent)
	for rows.Next() {
		var (
			roomID            string
			eventID           string
			streamPos         types.StreamPosition
			topoPos           types.StreamPosition
			eventBytes        []byte
			excludeFromSync   bool
			sessionID         *int64
			txnID             *string
			transactionID     *api.TransactionID
			historyVisibility gomatrixserverlib.HistoryVisibility
		)
		if err = rows.Scan(&roomID, &eventID, &streamPos, &eventBytes, &sessionID, &excludeFromSync, &txnID, &historyVisibility, &topoPos); err != nil {
			return nil, err
		}
		var ev rstypes.HeaderedEvent
		if err = json.Unmarshal(eventBytes, &ev); err != nil {
			return nil, err
		}
		if sessionID != nil && txnID != nil {
			transactionID = &api.TransactionID{SessionID: *sessionID, TransactionID: *txnID}
		}
		ev.Visibility = historyVisibility
		byRoom[roomID] = append(byRoom[roomID], topoEvent{
			StreamEvent: types.StreamEvent{
				HeaderedEvent:   &ev,
				StreamPosition:  streamPos,
				TransactionID:   transactionID,
				ExcludeFromSync: excludeFromSync,
			},
			topoPos: topoPos,
		})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}

	result := make(map[string]types.RecentEvents, len(byRoom))
	for roomID, evs := range byRoom {
		// Oldest first. The query returns the latest first; the extra event
		// it read beyond the limit is then the first one.
		sort.SliceStable(evs, func(i, j int) bool {
			if evs[i].topoPos != evs[j].topoPos {
				return evs[i].topoPos < evs[j].topoPos
			}
			return evs[i].StreamPosition < evs[j].StreamPosition
		})
		recent := types.RecentEvents{Topological: true}
		if len(evs) > eventFilter.Limit {
			recent.Limited = true
			evs = evs[len(evs)-eventFilter.Limit:]
		}
		recent.Events = make([]types.StreamEvent, len(evs))
		for i := range evs {
			recent.Events[i] = evs[i].StreamEvent
		}
		result[roomID] = recent
	}
	return result, nil
}
