package sqlite3

import (
	"database/sql"
	"fmt"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
)

type FilterOrder int

const (
	FilterOrderNone = iota
	FilterOrderAsc
	FilterOrderDesc
	// FilterOrderTopologyDesc orders the latest events of a room's topology
	// first, for a query on the topology table aliased as t.
	FilterOrderTopologyDesc
)

// prepareWithFilters returns a prepared statement with the
// relevant filters included. It also includes an []interface{}
// list of all the relevant parameters to pass straight to
// QueryContext, QueryRowContext etc.
// We don't take the filter object directly here because the
// fields might come from either a StateFilter or an EventFilter,
// and it's easier just to have the caller extract the relevant
// parts.
func prepareWithFilters(
	db *sql.DB, txn *sql.Tx, query string, params []any,
	senders, notsenders, types, nottypes *[]string, excludeEventIDs []string,
	containsURL *bool, limit int, order FilterOrder,
) (*sql.Stmt, []any, error) {
	offset := len(params)
	if senders != nil {
		if count := len(*senders); count > 0 {
			query += " AND sender IN " + sqlutil.QueryVariadicOffset(count, offset)
			for _, v := range *senders {
				params, offset = append(params, v), offset+1
			}
		} else {
			query += ` AND sender = ""`
		}
	}
	if notsenders != nil {
		if count := len(*notsenders); count > 0 {
			query += " AND sender NOT IN " + sqlutil.QueryVariadicOffset(count, offset)
			for _, v := range *notsenders {
				params, offset = append(params, v), offset+1
			}
		} else {
			query += ` AND sender != ""`
		}
	}
	if types != nil {
		if count := len(*types); count > 0 {
			query += " AND type IN " + sqlutil.QueryVariadicOffset(count, offset)
			for _, v := range *types {
				params, offset = append(params, v), offset+1
			}
		} else {
			query += ` AND type = ""`
		}
	}
	if nottypes != nil {
		if count := len(*nottypes); count > 0 {
			query += " AND type NOT IN " + sqlutil.QueryVariadicOffset(count, offset)
			for _, v := range *nottypes {
				params, offset = append(params, v), offset+1
			}
		} else {
			query += ` AND type != ""`
		}
	}
	if containsURL != nil {
		query += fmt.Sprintf(" AND contains_url = %v", *containsURL)
	}
	if count := len(excludeEventIDs); count > 0 {
		query += " AND event_id NOT IN " + sqlutil.QueryVariadicOffset(count, offset)
		for _, v := range excludeEventIDs {
			params, offset = append(params, v), offset+1
		}
	}
	switch order {
	case FilterOrderAsc:
		query += " ORDER BY id ASC"
	case FilterOrderDesc:
		query += " ORDER BY id DESC"
	case FilterOrderTopologyDesc:
		query += " ORDER BY t.topological_position DESC, t.stream_position DESC"
	}
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT $%d", offset+1)
		params = append(params, limit)
	}

	var stmt *sql.Stmt
	var err error
	if txn != nil {
		stmt, err = txn.Prepare(query)
	} else {
		stmt, err = db.Prepare(query)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("s.db.Prepare: %w", err)
	}
	return stmt, params, nil
}

// withRelationLabelFilters appends to query the conditions of a room event
// filter's relation types (MSC3874) and labels (MSC2326) on the event whose ID
// is in eventIDColumn, and to params their parameters, numbered after the
// existing ones. An event without a relation never matches rel_types and
// always passes not_rel_types, as in Synapse.
func withRelationLabelFilters(
	query string, params []any, filter *synctypes.RoomEventFilter, eventIDColumn string,
) (string, []any) {
	if filter == nil {
		return query, params
	}
	condition := func(values *[]string, not bool, from string) {
		if values == nil {
			return
		}
		exists := " AND EXISTS"
		if not {
			exists = " AND NOT EXISTS"
		}
		if len(*values) == 0 {
			if !not {
				// An empty list of values to keep matches nothing.
				query += " AND 0"
			}
			return
		}
		query += exists + " (" + fmt.Sprintf(from, eventIDColumn) + sqlutil.QueryVariadicOffset(len(*values), len(params)) + ")"
		for _, v := range *values {
			params = append(params, v)
		}
	}
	const relations = "SELECT 1 FROM syncapi_relations rel WHERE rel.child_event_id = %s AND rel.rel_type IN "
	const labels = "SELECT 1 FROM syncapi_event_labels lbl WHERE lbl.event_id = %s AND lbl.label IN "
	condition(filter.RelTypes, false, relations)
	condition(filter.NotRelTypes, true, relations)
	condition(filter.Labels, false, labels)
	condition(filter.NotLabels, true, labels)
	return query, params
}
