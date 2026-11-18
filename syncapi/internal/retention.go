// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"
	"fmt"
	"time"

	"codefloe.com/pat-s/zendrite/internal/retention"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
)

// retentionConfigured is implemented by the sync API database and its
// transactions, which carry the server's retention configuration. Test
// doubles that do not implement it apply no retention policy.
type retentionConfigured interface {
	RetentionConfig() *config.Retention
}

// retentionConfig returns the retention configuration of db, nil if it has
// none or retention policies are disabled.
func retentionConfig(db storage.DatabaseTransaction) *config.Retention {
	rc, ok := db.(retentionConfigured)
	if !ok {
		return nil
	}
	cfg := rc.RetentionConfig()
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	return cfg
}

// RoomRetentionPolicy returns the effective retention policy of a room,
// from its current m.room.retention event and the server's configuration.
func RoomRetentionPolicy(ctx context.Context, db storage.DatabaseTransaction, roomID string) (retention.Policy, error) {
	cfg := retentionConfig(db)
	if cfg == nil {
		return retention.Policy{}, nil
	}
	ev, err := db.GetStateEvent(ctx, roomID, retention.EventType, "")
	if err != nil {
		return retention.Policy{}, fmt.Errorf("db.GetStateEvent: %w", err)
	}
	if ev == nil {
		return retention.RoomPolicy(cfg, nil), nil
	}
	return retention.RoomPolicy(cfg, ev.PDU), nil
}

// RetentionFilter drops expired events, looking each room's policy up
// once.
type RetentionFilter struct {
	db       storage.DatabaseTransaction
	now      time.Time
	policies map[string]retention.Policy
}

// NewRetentionFilter returns a filter that reads policies from db, nil
// when retention policies are disabled. A nil filter drops nothing.
func NewRetentionFilter(db storage.DatabaseTransaction) *RetentionFilter {
	if retentionConfig(db) == nil {
		return nil
	}
	return &RetentionFilter{db: db, now: time.Now(), policies: make(map[string]retention.Policy)}
}

// Expired reports whether ev is past its room's retention policy.
func (f *RetentionFilter) Expired(ctx context.Context, ev *types.HeaderedEvent) (bool, error) {
	if f == nil {
		return false, nil
	}
	roomID := ev.RoomID().String()
	policy, ok := f.policies[roomID]
	if !ok {
		var err error
		if policy, err = RoomRetentionPolicy(ctx, f.db, roomID); err != nil {
			return false, err
		}
		f.policies[roomID] = policy
	}
	return policy.Expired(ev.PDU, f.now), nil
}

// Drop removes the expired events. The events may belong to different
// rooms.
func (f *RetentionFilter) Drop(ctx context.Context, events []*types.HeaderedEvent) ([]*types.HeaderedEvent, error) {
	if f == nil || len(events) == 0 {
		return events, nil
	}
	kept := events[:0:0]
	for _, ev := range events {
		expired, err := f.Expired(ctx, ev)
		if err != nil {
			return nil, err
		}
		if !expired {
			kept = append(kept, ev)
		}
	}
	return kept, nil
}

// DropExpiredEvents removes the events that are past their room's
// retention policy, which may no longer be served to clients. The events
// may belong to different rooms.
func DropExpiredEvents(ctx context.Context, db storage.DatabaseTransaction, events []*types.HeaderedEvent) ([]*types.HeaderedEvent, error) {
	return NewRetentionFilter(db).Drop(ctx, events)
}
