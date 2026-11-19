// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package retention

import (
	"context"
	"fmt"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
)

// Rooms looks up the retention policies of rooms in the roomserver for
// components outside the sync API, once per room. A nil *Rooms, which
// NewRooms returns while retention policies are disabled, expires nothing.
type Rooms struct {
	cfg   *config.Retention
	rsAPI api.QueryLatestEventsAndStateAPI
	now   time.Time
	rooms map[string]room
}

type room struct {
	policy      Policy
	extremities map[string]struct{}
}

// NewRooms returns a lookup that judges events at the time it is made, nil
// when retention policies are disabled.
func NewRooms(cfg *config.Retention, rsAPI api.QueryLatestEventsAndStateAPI) *Rooms {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	return &Rooms{cfg: cfg, rsAPI: rsAPI, now: time.Now(), rooms: make(map[string]room)}
}

func (r *Rooms) room(ctx context.Context, roomID string) (room, error) {
	if rm, ok := r.rooms[roomID]; ok {
		return rm, nil
	}
	var res api.QueryLatestEventsAndStateResponse
	if err := r.rsAPI.QueryLatestEventsAndState(ctx, &api.QueryLatestEventsAndStateRequest{
		RoomID:       roomID,
		StateToFetch: []gomatrixserverlib.StateKeyTuple{{EventType: EventType, StateKey: ""}},
	}, &res); err != nil {
		return room{}, fmt.Errorf("rsAPI.QueryLatestEventsAndState: %w", err)
	}
	var policyEvent gomatrixserverlib.PDU
	for _, ev := range res.StateEvents {
		if ev.Type() == EventType && ev.StateKeyEquals("") {
			policyEvent = ev.PDU
		}
	}
	rm := room{policy: RoomPolicy(r.cfg, policyEvent), extremities: make(map[string]struct{}, len(res.LatestEvents))}
	for _, id := range res.LatestEvents {
		rm.extremities[id] = struct{}{}
	}
	r.rooms[roomID] = rm
	return rm, nil
}

// Expired reports whether an event of the room, sent at originServerTS,
// is past the room's retention policy and must no longer be served to
// clients. State events never expire.
func (r *Rooms) Expired(ctx context.Context, roomID string, originServerTS spec.Timestamp, isState bool) (bool, error) {
	if r == nil || isState {
		return false, nil
	}
	rm, err := r.room(ctx, roomID)
	if err != nil {
		return false, err
	}
	return rm.policy.expiredAt(int64(originServerTS), r.now), nil
}

// Withheld reports whether ev must no longer be served to other servers:
// it has expired and is not a forward extremity of the room. The purge
// jobs keep forward extremities, and the MSC leaves the most recent event
// out of retention, so new events can still reference it in their
// prev_events.
func (r *Rooms) Withheld(ctx context.Context, ev gomatrixserverlib.PDU) (bool, error) {
	if r == nil || ev.StateKey() != nil {
		return false, nil
	}
	roomID := ev.RoomID().String()
	rm, err := r.room(ctx, roomID)
	if err != nil {
		return false, err
	}
	if !rm.policy.expiredAt(int64(ev.OriginServerTS()), r.now) {
		return false, nil
	}
	_, extremity := rm.extremities[ev.EventID()]
	return !extremity, nil
}
