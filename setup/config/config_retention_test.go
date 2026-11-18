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
	} {
		var errs ConfigErrors
		c.cfg.Verify(&errs)
		assert.Equal(t, c.valid, len(errs) == 0, "%s: %v", c.name, errs)
	}
}
