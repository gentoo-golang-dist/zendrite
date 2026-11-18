// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package retention

import (
	"math"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/stretchr/testify/assert"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
)

func TestValidateContent(t *testing.T) {
	for content, valid := range map[string]bool{
		`{}`: true,
		`{"max_lifetime":null,"min_lifetime":null}`: true,
		`{"max_lifetime":0}`:                        true,
		`{"max_lifetime":9007199254740991}`:         true,
		`{"min_lifetime":1000,"max_lifetime":1000}`: true,
		`{"max_lifetime":9007199254740992}`:         false,
		`{"max_lifetime":-1}`:                       false,
		`{"max_lifetime":1.5}`:                      false,
		`{"max_lifetime":1e3}`:                      false,
		`{"max_lifetime":"1000"}`:                   false,
		`{"min_lifetime":[]}`:                       false,
		`{"min_lifetime":1001,"max_lifetime":1000}`: false,
	} {
		err := ValidateContent([]byte(content))
		assert.Equal(t, valid, err == nil, "%s: %v", content, err)
	}
}

func TestMaxLifetime(t *testing.T) {
	assert.Nil(t, MaxLifetime([]byte(`{}`)))
	assert.Nil(t, MaxLifetime([]byte(`{"max_lifetime":"1d"}`)))
	assert.Equal(t, time.Hour, *MaxLifetime([]byte(`{"max_lifetime":3600000}`)))
	// A remote server's min_lifetime does not void a valid max_lifetime.
	assert.Equal(t, time.Second, *MaxLifetime([]byte(`{"min_lifetime":2000,"max_lifetime":1000}`)))
	// Lifetimes past what a time.Duration holds, about 292 years, saturate.
	assert.Equal(t, time.Duration(math.MaxInt64), *MaxLifetime([]byte(`{"max_lifetime":9007199254740991}`)))
}

func TestPolicyExpired(t *testing.T) {
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	now := time.Now()
	message := func(age time.Duration) gomatrixserverlib.PDU {
		return room.CreateEvent(t, alice, "m.room.message", map[string]any{"body": "hi"}, test.WithTimestamp(now.Add(-age))).PDU
	}
	retentionEvent := func(content map[string]any) gomatrixserverlib.PDU {
		return room.CreateEvent(t, alice, EventType, content, test.WithStateKey("")).PDU
	}
	old, recent := message(2*time.Hour), message(30*time.Minute)
	oldState := room.CreateEvent(t, alice, "m.room.topic", map[string]any{"topic": "x"}, test.WithStateKey(""), test.WithTimestamp(now.Add(-48*time.Hour))).PDU
	hourPolicy := retentionEvent(map[string]any{"max_lifetime": time.Hour.Milliseconds()})

	// Disabled, nothing expires.
	disabled := RoomPolicy(&config.Retention{DefaultPolicy: config.RetentionPolicy{MaxLifetime: time.Minute}}, hourPolicy)
	assert.False(t, disabled.Enforced())
	assert.False(t, disabled.Expired(old, now))

	// The room's policy.
	policy := RoomPolicy(&config.Retention{Enabled: true}, hourPolicy)
	assert.True(t, policy.Enforced())
	assert.True(t, policy.Expired(old, now))
	assert.False(t, policy.Expired(recent, now))
	assert.False(t, policy.Expired(oldState, now), "state events never expire")

	// No room policy and no default policy.
	assert.False(t, RoomPolicy(&config.Retention{Enabled: true}, nil).Enforced())
	assert.False(t, RoomPolicy(&config.Retention{Enabled: true}, retentionEvent(map[string]any{"min_lifetime": 1000})).Enforced())

	// The default policy fills in a missing max_lifetime.
	withDefault := &config.Retention{Enabled: true, DefaultPolicy: config.RetentionPolicy{MaxLifetime: 20 * time.Minute}}
	assert.True(t, RoomPolicy(withDefault, nil).Expired(recent, now))
	assert.True(t, RoomPolicy(withDefault, retentionEvent(map[string]any{"min_lifetime": 1000})).Expired(recent, now))
	assert.False(t, RoomPolicy(withDefault, hourPolicy).Expired(recent, now))

	// The allowed range bounds the room's max_lifetime.
	assert.False(t, RoomPolicy(&config.Retention{Enabled: true, AllowedLifetimeMin: 3 * time.Hour}, hourPolicy).Expired(old, now))
	assert.True(t, RoomPolicy(&config.Retention{Enabled: true, AllowedLifetimeMax: 10 * time.Minute}, hourPolicy).Expired(recent, now))
}

func TestConfigMaxLifetime(t *testing.T) {
	hour, day := time.Hour, 24*time.Hour
	cfg := &config.Retention{Enabled: true, DefaultPolicy: config.RetentionPolicy{MaxLifetime: day}, AllowedLifetimeMin: 2 * hour, AllowedLifetimeMax: 2 * day}
	for _, c := range []struct {
		room *time.Duration
		want time.Duration
	}{
		{nil, day},
		{&hour, 2 * hour},
		{ptr(10 * day), 2 * day},
		{ptr(36 * hour), 36 * hour},
	} {
		got, ok := cfg.MaxLifetime(c.room)
		assert.True(t, ok)
		assert.Equal(t, c.want, got)
	}
	_, ok := (&config.Retention{Enabled: true, AllowedLifetimeMax: day}).MaxLifetime(nil)
	assert.False(t, ok, "limits alone set no policy")
}

func ptr(d time.Duration) *time.Duration { return &d }
