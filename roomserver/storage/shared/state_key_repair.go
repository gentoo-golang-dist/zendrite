// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package shared

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// MiskeyedStateEvent is a state event stored under a state key NID that does
// not match the state key in its JSON.
type MiskeyedStateEvent struct {
	EventNID     types.EventNID
	RoomNID      types.RoomNID
	EventTypeNID types.EventTypeNID
	// OldStateKeyNID is the state key NID the event is stored under. It is 0
	// if the event was stored as if it were not a state event.
	OldStateKeyNID types.EventStateKeyNID
	// StateKey is the state key in the event JSON.
	StateKey string
}

// StateKeyRepairCursor returns the event NID up to which the repair of state
// events stored under the wrong state key NID has checked events, and false
// if the repair is not pending.
func (d *Database) StateKeyRepairCursor(ctx context.Context) (types.EventNID, bool, error) {
	return d.StateKeyRepairTable.SelectStateKeyRepairCursor(ctx, nil)
}

// MiskeyedStateEventsAfter checks up to limit events after afterEventNID
// whose type can be an auth event, and returns the ones stored under a state
// key NID that does not match the state key in their JSON. It also returns
// the last event NID it checked, and whether there are no more events to
// check.
func (d *Database) MiskeyedStateEventsAfter(
	ctx context.Context, afterEventNID types.EventNID, limit int,
) (miskeyed []MiskeyedStateEvent, lastEventNID types.EventNID, done bool, err error) {
	events, err := d.StateKeyRepairTable.SelectAuthTypeEventsAfter(ctx, nil, afterEventNID, limit)
	if err != nil {
		return nil, 0, false, fmt.Errorf("d.StateKeyRepairTable.SelectAuthTypeEventsAfter: %w", err)
	}
	if len(events) == 0 {
		return nil, afterEventNID, true, nil
	}
	stateKeyNIDs := make([]types.EventStateKeyNID, 0, len(events))
	for _, ev := range events {
		if ev.EventStateKeyNID != 0 {
			stateKeyNIDs = append(stateKeyNIDs, ev.EventStateKeyNID)
		}
	}
	stateKeys, err := d.EventStateKeys(ctx, stateKeyNIDs)
	if err != nil {
		return nil, 0, false, fmt.Errorf("d.EventStateKeys: %w", err)
	}
	for _, ev := range events {
		stateKey := gjson.GetBytes(ev.EventJSON, "state_key")
		if stateKey.Type != gjson.String {
			// Not a state event, which an event of these types cannot be
			// unless its JSON is invalid. Leave it alone.
			continue
		}
		if stored, ok := stateKeys[ev.EventStateKeyNID]; ok && ev.EventStateKeyNID != 0 && stored == stateKey.String() {
			continue
		}
		miskeyed = append(miskeyed, MiskeyedStateEvent{
			EventNID:       ev.EventNID,
			RoomNID:        ev.RoomNID,
			EventTypeNID:   ev.EventTypeNID,
			OldStateKeyNID: ev.EventStateKeyNID,
			StateKey:       stateKey.String(),
		})
	}
	return miskeyed, events[len(events)-1].EventNID, len(events) < limit, nil
}

// AdvanceStateKeyRepair records that the repair has checked events up to
// afterEventNID, or ends the repair if done is set.
func (d *Database) AdvanceStateKeyRepair(ctx context.Context, afterEventNID types.EventNID, done bool) error {
	return d.Writer.Do(d.DB, nil, func(txn *sql.Tx) error {
		if done {
			return d.StateKeyRepairTable.DeleteStateKeyRepair(ctx, txn)
		}
		return d.StateKeyRepairTable.UpdateStateKeyRepairCursor(ctx, txn, afterEventNID)
	})
}
