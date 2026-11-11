// Copyright 2024 New Vector Ltd.
// Copyright 2017 Thibaut CHARLES
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package postgres

import (
	"fmt"
	"strings"

	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
)

// filterConvertWildcardToSQL converts wildcards as defined in
// https://matrix.org/docs/spec/client_server/r0.3.0.html#post-matrix-client-r0-user-userid-filter
// to SQL wildcards that can be used with LIKE().
func filterConvertTypeWildcardToSQL(values *[]string) []string {
	if values == nil {
		// Return nil instead of []string{} so IS NULL can work correctly when
		// the return value is passed into SQL queries
		return nil
	}

	v := *values
	ret := make([]string, len(v))
	for i := range v {
		ret[i] = strings.ReplaceAll(v[i], "*", "%")
	}
	return ret
}

// TODO: Replace when Zendrite uses Go 1.18
func getSendersRoomEventFilter(filter *synctypes.RoomEventFilter) (senders []string, notSenders []string) {
	if filter.Senders != nil {
		senders = *filter.Senders
	}
	if filter.NotSenders != nil {
		notSenders = *filter.NotSenders
	}
	return senders, notSenders
}

func getSendersStateFilterFilter(filter *synctypes.StateFilter) (senders []string, notSenders []string) {
	if filter.Senders != nil {
		senders = *filter.Senders
	}
	if filter.NotSenders != nil {
		notSenders = *filter.NotSenders
	}
	return senders, notSenders
}

// optionalStrings passes a filter list to a query: NULL when the filter
// leaves it out, and an array, possibly empty, when it is set.
func optionalStrings(values *[]string) []string {
	if values == nil {
		return nil
	}
	if *values == nil {
		return []string{}
	}
	return *values
}

// relationLabelFilterSQL returns the conditions of a room event filter's
// relation types (MSC3874) and labels (MSC2326) on the event whose ID is in
// eventIDColumn, taking the lists from relationLabelFilterArgs as the
// parameters numbered from first. An event without a relation never matches
// rel_types and always passes not_rel_types, as in Synapse.
func relationLabelFilterSQL(eventIDColumn string, first int) string {
	conditions := []string{
		"$%[2]d::text[] IS NULL OR EXISTS (SELECT 1 FROM syncapi_relations rel WHERE rel.child_event_id = %[1]s AND rel.rel_type = ANY($%[2]d))",
		"$%[2]d::text[] IS NULL OR NOT EXISTS (SELECT 1 FROM syncapi_relations rel WHERE rel.child_event_id = %[1]s AND rel.rel_type = ANY($%[2]d))",
		"$%[2]d::text[] IS NULL OR EXISTS (SELECT 1 FROM syncapi_event_labels lbl WHERE lbl.event_id = %[1]s AND lbl.label = ANY($%[2]d))",
		"$%[2]d::text[] IS NULL OR NOT EXISTS (SELECT 1 FROM syncapi_event_labels lbl WHERE lbl.event_id = %[1]s AND lbl.label = ANY($%[2]d))",
	}
	var sql string
	for i, condition := range conditions {
		sql += " AND ( " + fmt.Sprintf(condition, eventIDColumn, first+i) + " )"
	}
	return sql
}

// relationLabelFilterArgs returns the parameters of relationLabelFilterSQL.
func relationLabelFilterArgs(filter *synctypes.RoomEventFilter) []any {
	return []any{
		optionalStrings(filter.RelTypes),
		optionalStrings(filter.NotRelTypes),
		optionalStrings(filter.Labels),
		optionalStrings(filter.NotLabels),
	}
}

// withRelationLabelArgs appends the parameters of relationLabelFilterSQL to
// a query's other parameters.
func withRelationLabelArgs(filter *synctypes.RoomEventFilter, args ...any) []any {
	return append(args, relationLabelFilterArgs(filter)...)
}
