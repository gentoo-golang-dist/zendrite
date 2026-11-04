// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"codefloe.com/pat-s/gomatrixserverlib"

	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// maxRejectedAuthEvents bounds how many rejected events one check looks at, as
// a remote server decides how long a chain of them is. A chain beyond it counts
// as rejected.
const maxRejectedAuthEvents = 64

// authEventStore reads events and whether they were rejected, from the database
// or from the transaction of a repair.
type authEventStore interface {
	EventsFromIDs(ctx context.Context, roomInfo *types.RoomInfo, eventIDs []string) ([]types.Event, error)
	IsEventRejected(ctx context.Context, roomNID types.RoomNID, eventID string) (bool, error)
}

// rejectedAuthEvent returns the first of the event's auth events that was
// rejected, or an empty string if there is none. The spec rejects an event that
// cites a rejected event.
//
// An event is also stored as rejected when its prev events could not be
// fetched, which says nothing about it: a power level event rejected that way
// must not take every later message of the room with it. Such an event passes
// against its own auth events and is not returned. An event whose auth events
// are missing cannot be verified, and is returned.
func (r *Inputer) rejectedAuthEvent(
	ctx context.Context, store authEventStore, roomInfo *types.RoomInfo, event gomatrixserverlib.PDU,
) (string, error) {
	checked := map[string]bool{}
	for _, authEventID := range event.AuthEventIDs() {
		stands, err := r.rejectionStands(ctx, store, roomInfo, authEventID, checked)
		if err != nil {
			return "", err
		}
		if stands {
			return authEventID, nil
		}
	}
	return "", nil
}

// rejectionStands reports whether the given event is stored as rejected and
// fails its own auth events, including by citing such an event. An event this
// server does not have is not reported, as the caller decides about those.
func (r *Inputer) rejectionStands(
	ctx context.Context, store authEventStore, roomInfo *types.RoomInfo, eventID string, checked map[string]bool,
) (bool, error) {
	if stands, ok := checked[eventID]; ok {
		return stands, nil
	}
	rejected, err := store.IsEventRejected(ctx, roomInfo.RoomNID, eventID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !rejected) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store.IsEventRejected: %w", err)
	}
	if len(checked) >= maxRejectedAuthEvents {
		return true, nil
	}
	// A cycle cannot pass, and auth events cannot form one in a valid room.
	checked[eventID] = true

	events, err := store.EventsFromIDs(ctx, roomInfo, []string{eventID})
	if err != nil || len(events) == 0 || events[0].PDU == nil {
		return true, nil //nolint:nilerr // an event that cannot be loaded cannot be verified
	}
	event := events[0].PDU

	authEventIDs := event.AuthEventIDs()
	authEvents := make([]gomatrixserverlib.PDU, 0, len(authEventIDs))
	for _, authEventID := range authEventIDs {
		stands, serr := r.rejectionStands(ctx, store, roomInfo, authEventID, checked)
		if serr != nil {
			return false, serr
		}
		if stands {
			return true, nil
		}
		authEvent, serr := store.EventsFromIDs(ctx, roomInfo, []string{authEventID})
		if serr != nil || len(authEvent) == 0 || authEvent[0].PDU == nil {
			return true, nil //nolint:nilerr // an event with missing auth events cannot be verified
		}
		authEvents = append(authEvents, authEvent[0].PDU)
	}
	allowed, err := r.allowedBy(ctx, event, authEvents)
	if err != nil {
		return false, err
	}
	checked[eventID] = !allowed
	return !allowed, nil
}
