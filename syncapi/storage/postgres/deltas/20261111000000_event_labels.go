// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package deltas

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
)

const insertEventLabelSQL = "" +
	"INSERT INTO syncapi_event_labels (event_id, room_id, label) VALUES ($1, $2, $3)" +
	" ON CONFLICT ON CONSTRAINT syncapi_event_labels_unique DO NOTHING"

// UpEventLabels stores the labels (MSC2326) of the events stored before the
// syncapi_event_labels table existed. Only events whose JSON names the field are read.
func UpEventLabels(ctx context.Context, txn *sql.Tx) error {
	rows, err := txn.QueryContext(ctx,
		"SELECT event_id, room_id, headered_event_json FROM syncapi_output_room_events"+
			" WHERE headered_event_json LIKE '%org.matrix.labels%'")
	if err != nil {
		return fmt.Errorf("select labeled events: %w", err)
	}
	type labeled struct {
		eventID, roomID string
		labels          []string
	}
	var events []labeled
	for rows.Next() {
		var eventID, roomID string
		var eventJSON []byte
		if err = rows.Scan(&eventID, &roomID, &eventJSON); err != nil {
			internal.CloseAndLogIfError(ctx, rows, "UpEventLabels: rows.close() failed")
			return err
		}
		if labels := synctypes.EventLabels([]byte(gjson.GetBytes(eventJSON, "content").Raw)); len(labels) > 0 {
			events = append(events, labeled{eventID, roomID, labels})
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, ev := range events {
		for _, label := range ev.labels {
			if _, err = txn.ExecContext(ctx, insertEventLabelSQL, ev.eventID, ev.roomID, label); err != nil {
				return fmt.Errorf("insert event label: %w", err)
			}
		}
	}
	return nil
}
