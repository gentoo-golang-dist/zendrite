// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"context"

	"codefloe.com/pat-s/zendrite/internal/retention"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
)

// retentionPurgeBatch is how many candidate events a purge transaction
// looks at, so that the room's lock is held briefly.
const retentionPurgeBatch = 500

// retentionPass purges expired events from the roomserver (MSC1763): the
// non-state events of each room past its effective max_lifetime, except
// the room's forward extremities and the last event sent to the output log.
//
// It only runs in the activation that owns the homeserver: components only
// start there, a retired owner's database writes are fenced, and a pass
// stops at the first batch after ownership is lost. Each batch is its own
// transaction and only deletes expired events, so a pass cut short by a
// takeover leaves consistent storage, and the successor's first pass
// carries on.
func (r *Inputer) retentionPass() retention.Pass {
	owns := func() bool { return jetstream.OwnsHomeserver(r.JetStream) }
	return retention.Pass{
		Component: "roomserver",
		RoomIDs:   r.DB.RetentionRoomIDs,
		Policy: func(ctx context.Context, roomID string) (retention.Policy, error) {
			ev, err := r.DB.GetStateEvent(ctx, roomID, retention.EventType, "")
			if err != nil || ev == nil {
				return retention.RoomPolicy(&r.Cfg.Matrix.Retention, nil), err
			}
			return retention.RoomPolicy(&r.Cfg.Matrix.Retention, ev.PDU), nil
		},
		PurgeRoom: func(ctx context.Context, roomID string, cutoff int64) (int, error) {
			var after types.EventNID
			total := 0
			for ctx.Err() == nil && owns() {
				purged, next, done, err := r.DB.PurgeExpiredEvents(ctx, roomID, cutoff, after, retentionPurgeBatch)
				total += len(purged)
				if err != nil || done {
					return total, err
				}
				after = next
			}
			return total, ctx.Err()
		},
		Owns: owns,
	}
}
