// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// Package retention reads message retention policies (MSC1763).
package retention

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// EventType is the state event that sets a room's retention policy, with
// an empty state key. Synapse and Element use it rather than the MSC's
// unstable org.matrix.msc1763.retention.
const EventType = "m.room.retention"

// maxLifetimeMS is the largest lifetime the MSC allows, 2^53-1 ms.
const maxLifetimeMS = 1<<53 - 1

// ValidateContent checks the content of an m.room.retention event a local
// user sends: min_lifetime and max_lifetime must each be absent, null, or
// an integer in [0, 2^53-1], and min_lifetime must not exceed max_lifetime.
func ValidateContent(content []byte) error {
	minLifetime, err := lifetimeMS(content, "min_lifetime")
	if err != nil {
		return err
	}
	maxLifetime, err := lifetimeMS(content, "max_lifetime")
	if err != nil {
		return err
	}
	if minLifetime != nil && maxLifetime != nil && *minLifetime > *maxLifetime {
		return errors.New("'min_lifetime' can't be greater than 'max_lifetime'")
	}
	return nil
}

// MaxLifetime returns the max_lifetime an m.room.retention event's content
// sets, or nil if it sets none. A value that is not a valid lifetime, which
// a remote server may send, counts as none.
func MaxLifetime(content []byte) *time.Duration {
	ms, err := lifetimeMS(content, "max_lifetime")
	if err != nil || ms == nil {
		return nil
	}
	var d time.Duration
	if *ms > math.MaxInt64/int64(time.Millisecond) {
		d = time.Duration(math.MaxInt64)
	} else {
		d = time.Duration(*ms) * time.Millisecond
	}
	return &d
}

// lifetimeMS reads the lifetime under key, nil if it is absent or null.
func lifetimeMS(content []byte, key string) (*int64, error) {
	v := gjson.GetBytes(content, key)
	if !v.Exists() || v.Type == gjson.Null {
		return nil, nil //nolint:nilnil
	}
	if v.Type != gjson.Number {
		return nil, fmt.Errorf("'%s' must be an integer", key)
	}
	ms, err := strconv.ParseInt(v.Raw, 10, 64)
	if err != nil || ms < 0 || ms > maxLifetimeMS {
		return nil, fmt.Errorf("'%s' must be an integer between 0 and 2^53-1", key)
	}
	return &ms, nil
}

// Policy is the effective max_lifetime of a room, worked out once and then
// applied to its events.
type Policy struct {
	maxLifetime time.Duration
	enforced    bool
}

// RoomPolicy returns the effective policy of a room whose current
// m.room.retention event is ev, nil if it has none.
func RoomPolicy(cfg *config.Retention, ev gomatrixserverlib.PDU) Policy {
	var roomMax *time.Duration
	if ev != nil {
		roomMax = MaxLifetime(ev.Content())
	}
	maxLifetime, enforced := cfg.MaxLifetime(roomMax)
	return Policy{maxLifetime: maxLifetime, enforced: enforced}
}

// Enforced reports whether any event of the room can expire.
func (p Policy) Enforced() bool { return p.enforced }

// MaxLifetime is the room's effective max_lifetime, if Enforced.
func (p Policy) MaxLifetime() time.Duration { return p.maxLifetime }

// Cutoff returns the origin_server_ts before which the room's non-state
// events have expired at now, or false if none expire.
func (p Policy) Cutoff(now time.Time) (int64, bool) {
	if !p.enforced {
		return 0, false
	}
	return now.UnixMilli() - p.maxLifetime.Milliseconds(), true
}

// Expired reports whether ev is past the room's max_lifetime at now and
// must no longer be served. State events never expire.
func (p Policy) Expired(ev gomatrixserverlib.PDU, now time.Time) bool {
	if ev.StateKey() != nil {
		return false
	}
	return p.expiredAt(int64(ev.OriginServerTS()), now)
}

// expiredAt reports whether a non-state event sent at originServerTS is
// past the room's max_lifetime at now.
func (p Policy) expiredAt(originServerTS int64, now time.Time) bool {
	cutoff, ok := p.Cutoff(now)
	return ok && originServerTS < cutoff
}
