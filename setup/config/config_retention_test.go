// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRetentionVerify(t *testing.T) {
	for _, c := range []struct {
		name  string
		cfg   Retention
		valid bool
	}{
		{"unset", Retention{}, true},
		{"full", Retention{Enabled: true, DefaultPolicy: RetentionPolicy{MinLifetime: time.Hour, MaxLifetime: 24 * time.Hour}, AllowedLifetimeMin: time.Hour, AllowedLifetimeMax: 48 * time.Hour}, true},
		{"default min above max", Retention{DefaultPolicy: RetentionPolicy{MinLifetime: 2 * time.Hour, MaxLifetime: time.Hour}}, false},
		{"allowed min above max", Retention{AllowedLifetimeMin: 2 * time.Hour, AllowedLifetimeMax: time.Hour}, false},
		{"negative", Retention{AllowedLifetimeMax: -time.Hour}, false},
		{"purge jobs", Retention{PurgeJobs: []RetentionPurgeJob{{LongestMaxLifetime: 72 * time.Hour, Interval: 12 * time.Hour}, {ShortestMaxLifetime: 72 * time.Hour, Interval: 24 * time.Hour}}}, true},
		{"purge job without interval", Retention{PurgeJobs: []RetentionPurgeJob{{LongestMaxLifetime: time.Hour}}}, false},
		{"purge job range empty", Retention{PurgeJobs: []RetentionPurgeJob{{ShortestMaxLifetime: time.Hour, LongestMaxLifetime: time.Hour, Interval: time.Hour}}}, false},
		{"purge job range reversed", Retention{PurgeJobs: []RetentionPurgeJob{{ShortestMaxLifetime: 2 * time.Hour, LongestMaxLifetime: time.Hour, Interval: time.Hour}}}, false},
	} {
		var errs ConfigErrors
		c.cfg.Verify(&errs)
		assert.Equal(t, c.valid, len(errs) == 0, "%s: %v", c.name, errs)
	}
}

func TestRetentionPurgeJobs(t *testing.T) {
	assert.Equal(t, []RetentionPurgeJob{{Interval: DefaultRetentionPurgeInterval}}, (&Retention{}).Jobs())
	short := RetentionPurgeJob{LongestMaxLifetime: 72 * time.Hour}
	long := RetentionPurgeJob{ShortestMaxLifetime: 72 * time.Hour}
	for _, c := range []struct {
		lifetime    time.Duration
		short, long bool
	}{
		{time.Hour, true, false},
		{72 * time.Hour, true, false},
		{73 * time.Hour, false, true},
	} {
		assert.Equal(t, c.short, short.Covers(c.lifetime), c.lifetime)
		assert.Equal(t, c.long, long.Covers(c.lifetime), c.lifetime)
	}
	assert.True(t, RetentionPurgeJob{}.Covers(0))
}
