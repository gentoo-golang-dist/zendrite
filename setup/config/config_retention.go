// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package config

import (
	"time"
)

// Retention configures message retention policies (MSC1763): how long the
// non-state events of a room are kept, set by the room's m.room.retention
// state event and by the server. Its shape follows Synapse's `retention`
// section, with Go durations.
type Retention struct {
	// Enabled turns on retention policies. While it is off, m.room.retention
	// events have no effect on what Zendrite serves or keeps.
	Enabled bool `yaml:"enabled"`

	// DefaultPolicy applies to rooms without an m.room.retention event, and
	// fills in a lifetime the room's event leaves out.
	DefaultPolicy RetentionPolicy `yaml:"default_policy"`

	// AllowedLifetimeMin and AllowedLifetimeMax bound the max_lifetime a room
	// may ask for: a shorter one is raised to AllowedLifetimeMin, a longer
	// one lowered to AllowedLifetimeMax. Zero leaves that side unbounded.
	AllowedLifetimeMin time.Duration `yaml:"allowed_lifetime_min"`
	AllowedLifetimeMax time.Duration `yaml:"allowed_lifetime_max"`
}

// RetentionPolicy is a server-side retention policy. A zero lifetime is not
// set.
type RetentionPolicy struct {
	// MinLifetime is reported to clients but, as in Synapse, not enforced.
	MinLifetime time.Duration `yaml:"min_lifetime"`
	// MaxLifetime is how long after its origin_server_ts an event is served
	// and kept.
	MaxLifetime time.Duration `yaml:"max_lifetime"`
}

// Verify checks that the lifetimes are consistent.
func (c *Retention) Verify(configErrs *ConfigErrors) {
	checkNonNegativeDuration := func(key string, d time.Duration) {
		if d < 0 {
			configErrs.Add("global.retention." + key + " must not be negative")
		}
	}
	checkNonNegativeDuration("default_policy.min_lifetime", c.DefaultPolicy.MinLifetime)
	checkNonNegativeDuration("default_policy.max_lifetime", c.DefaultPolicy.MaxLifetime)
	checkNonNegativeDuration("allowed_lifetime_min", c.AllowedLifetimeMin)
	checkNonNegativeDuration("allowed_lifetime_max", c.AllowedLifetimeMax)
	if c.DefaultPolicy.MinLifetime > 0 && c.DefaultPolicy.MaxLifetime > 0 && c.DefaultPolicy.MinLifetime > c.DefaultPolicy.MaxLifetime {
		configErrs.Add("global.retention.default_policy.min_lifetime must not be greater than its max_lifetime")
	}
	if c.AllowedLifetimeMin > 0 && c.AllowedLifetimeMax > 0 && c.AllowedLifetimeMin > c.AllowedLifetimeMax {
		configErrs.Add("global.retention.allowed_lifetime_min must not be greater than allowed_lifetime_max")
	}
}

// MaxLifetime returns the effective max_lifetime of a room whose
// m.room.retention event sets roomMaxLifetime, nil if the room has no such
// event or the event leaves max_lifetime out. The second result is false if
// the room's events never expire: retention is off, or neither the room nor
// the default policy sets a max_lifetime. The lifetime is bounded by
// AllowedLifetimeMin and AllowedLifetimeMax, both when serving events and
// when purging them.
func (c *Retention) MaxLifetime(roomMaxLifetime *time.Duration) (time.Duration, bool) {
	if c == nil || !c.Enabled {
		return 0, false
	}
	var lifetime time.Duration
	switch {
	case roomMaxLifetime != nil:
		lifetime = *roomMaxLifetime
	case c.DefaultPolicy.MaxLifetime > 0:
		lifetime = c.DefaultPolicy.MaxLifetime
	default:
		return 0, false
	}
	if c.AllowedLifetimeMin > 0 && lifetime < c.AllowedLifetimeMin {
		lifetime = c.AllowedLifetimeMin
	}
	if c.AllowedLifetimeMax > 0 && lifetime > c.AllowedLifetimeMax {
		lifetime = c.AllowedLifetimeMax
	}
	return lifetime, true
}
