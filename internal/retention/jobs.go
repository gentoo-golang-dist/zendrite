// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package retention

import (
	"context"
	"time"

	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// FirstPurgeDelay is how long after start a purge job first runs. The first
// pass does not wait a whole interval, so that an activation that is
// replaced more often than its jobs' interval, as under HA, still purges.
// Every pass only deletes what has expired, so running it again after a
// takeover is harmless.
const FirstPurgeDelay = time.Minute

// RunPurgeJobs starts, with spawn, a loop for each purge job of cfg, if
// retention policies are enabled, that calls purge with the job
// FirstPurgeDelay after start and then every interval until ctx is done. A
// pass runs to completion before the next is scheduled, so passes of one
// job never overlap.
func RunPurgeJobs(ctx context.Context, cfg *config.Retention, spawn func(func()), purge func(context.Context, config.RetentionPurgeJob)) {
	if cfg == nil || !cfg.Enabled {
		return
	}
	firstDelay := FirstPurgeDelay
	if cfg.FirstPurgeDelay > 0 {
		firstDelay = cfg.FirstPurgeDelay
	}
	for _, job := range cfg.Jobs() {
		spawn(func() {
			timer := time.NewTimer(firstDelay)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				}
				purge(ctx, job)
				timer.Reset(job.Interval)
			}
		})
	}
}

// Pass is one component's purge of its own tables. The roomserver and the
// sync API each run their own, without waiting on a message from the
// other, so a takeover between the two leaves nothing that the next pass
// does not catch up on.
type Pass struct {
	// Component names the component in logs.
	Component string
	// RoomIDs lists the rooms to look at.
	RoomIDs func(ctx context.Context) ([]string, error)
	// Policy returns a room's effective policy.
	Policy func(ctx context.Context, roomID string) (Policy, error)
	// PurgeRoom deletes the room's non-state events sent before cutoff and
	// returns how many it deleted. It checks Owns before each batch.
	PurgeRoom func(ctx context.Context, roomID string, cutoff int64) (int, error)
	// Owns reports whether this process may still write. A pass stops at
	// the first room or batch after it turns false.
	Owns func() bool
}

// Run purges the expired events of every room that job covers.
func (p Pass) Run(ctx context.Context, job config.RetentionPurgeJob) {
	log := logrus.WithField("component", p.Component)
	roomIDs, err := p.RoomIDs(ctx)
	if err != nil {
		log.WithError(err).Warn("Retention purge: failed to list rooms")
		return
	}
	start := time.Now()
	var rooms, events int
	stopped := false
	for _, roomID := range roomIDs {
		if ctx.Err() != nil || !p.Owns() {
			stopped = true
			break
		}
		policy, err := p.Policy(ctx, roomID)
		if err != nil {
			log.WithError(err).WithField("room_id", roomID).Warn("Retention purge: failed to read the room's policy")
			continue
		}
		if !policy.Enforced() || !job.Covers(policy.MaxLifetime()) {
			continue
		}
		cutoff, _ := policy.Cutoff(time.Now())
		purged, err := p.PurgeRoom(ctx, roomID, cutoff)
		if purged > 0 {
			rooms++
			events += purged
		}
		if err != nil {
			log.WithError(err).WithField("room_id", roomID).Warn("Retention purge: failed to purge the room's expired events")
		}
	}
	log = log.WithFields(logrus.Fields{"rooms": rooms, "events": events, "duration": time.Since(start)})
	switch {
	case stopped:
		log.Info("Retention purge: pass stopped early, ownership lost or shutting down")
	case events > 0:
		log.Info("Retention purge: purged expired events")
	default:
		log.Debug("Retention purge: no expired events")
	}
}
