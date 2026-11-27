// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/syncapi/storage"
)

// StateDeltaCompactionInterval is how often CompactStateDeltas looks at every
// room again. A pass over rooms that are already compact reads only their
// state change rows.
const StateDeltaCompactionInterval = 24 * time.Hour

// RunStateDeltaCompaction removes redundant re-added state event IDs from the
// sync API's rows of every room, first after firstDelay and then every
// interval, while owns reports that this process may write.
func RunStateDeltaCompaction(ctx context.Context, db storage.Database, owns func() bool, firstDelay, interval time.Duration) {
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if owns() {
			CompactStateDeltas(ctx, db, owns)
		}
		timer.Reset(interval)
	}
}

// CompactStateDeltas makes one pass of state delta compaction over every room.
func CompactStateDeltas(ctx context.Context, db storage.Database, owns func() bool) {
	log := logrus.WithField("component", "syncapi")
	roomIDs, err := db.RetentionRoomIDs(ctx)
	if err != nil {
		log.WithError(err).Warn("State delta compaction: failed to list rooms")
		return
	}
	snapshot, err := db.NewDatabaseSnapshot(ctx)
	if err != nil {
		log.WithError(err).Warn("State delta compaction: failed to read the stream position")
		return
	}
	maxID, err := snapshot.MaxStreamPositionForPDUs(ctx)
	_ = snapshot.Rollback()
	if err != nil {
		log.WithError(err).Warn("State delta compaction: failed to read the stream position")
		return
	}
	start := time.Now()
	var rooms, rows, ids int
	for _, roomID := range roomIDs {
		if ctx.Err() != nil || !owns() {
			log.WithFields(logrus.Fields{"rooms": rooms, "rows": rows, "state_ids": ids}).Info("State delta compaction: pass stopped early, ownership lost or shutting down")
			return
		}
		roomRows, roomIDsRemoved, err := db.CompactStateDeltas(ctx, roomID, maxID, owns)
		if roomRows > 0 {
			rooms++
			rows += roomRows
			ids += roomIDsRemoved
		}
		if err != nil {
			log.WithError(err).WithField("room_id", roomID).Warn("State delta compaction: failed to compact the room")
		}
	}
	log = log.WithFields(logrus.Fields{"rooms": rooms, "rows": rows, "state_ids": ids, "duration": time.Since(start)})
	if ids > 0 {
		// The database reuses the space; returning it to the filesystem needs a
		// rewrite of the table, which locks it, so it is left to the operator.
		log.WithField("reclaim", "https://docs.zendrite.xyz/administration/optimisation/#reclaim-disk-space").
			Info("State delta compaction: removed redundant state event IDs")
	} else {
		log.Debug("State delta compaction: nothing to remove")
	}
}
