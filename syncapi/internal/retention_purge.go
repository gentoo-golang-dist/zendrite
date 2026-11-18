// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"

	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/internal/fulltext"
	"codefloe.com/pat-s/zendrite/internal/retention"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/storage/shared"
)

// retentionPurgeBatch is how many events a purge transaction looks at.
const retentionPurgeBatch = 500

// RetentionPass purges expired events from the sync API's own tables and
// from the search index (MSC1763), independently of the roomserver. Index
// is nil when search is disabled, and owns reports whether this process
// may still write.
func RetentionPass(db storage.Database, index fulltext.Indexer, owns func() bool) retention.Pass {
	return retention.Pass{
		Component: "syncapi",
		RoomIDs:   db.RetentionRoomIDs,
		Policy: func(ctx context.Context, roomID string) (retention.Policy, error) {
			snapshot, err := db.NewDatabaseSnapshot(ctx)
			if err != nil {
				return retention.Policy{}, err
			}
			defer snapshot.Rollback() //nolint:errcheck
			return RoomRetentionPolicy(ctx, snapshot, roomID)
		},
		PurgeRoom: func(ctx context.Context, roomID string, cutoff int64) (int, error) {
			cursor := shared.RetentionCursor{TS: cutoff - 1}
			total := 0
			for ctx.Err() == nil && owns() {
				purged, next, done, err := db.PurgeExpiredEvents(ctx, roomID, cursor, retentionPurgeBatch)
				total += len(purged)
				if index != nil {
					for _, eventID := range purged {
						if indexErr := index.Delete(eventID); indexErr != nil {
							logrus.WithError(indexErr).WithField("event_id", eventID).Warn("Retention purge: failed to remove an event from the search index")
						}
					}
				}
				if err != nil || done {
					return total, err
				}
				cursor = next
			}
			return total, ctx.Err()
		},
		Owns: owns,
	}
}
