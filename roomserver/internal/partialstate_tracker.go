// Copyright 2025 Jackmaninov
// Copyright 2025 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"
	"sync"
)

// PartialStateTracker lets requests that need the full state of a room wait
// for the room's partial-state resync to finish (MSC3902).
//
// Waiters live in memory. The resync runs on the server that holds HA
// ownership, which also serves the requests, and a takeover drops both; the
// partial-state flag itself is stored, so a waiter registered on the new
// owner sees it.
type PartialStateTracker struct {
	mu            sync.Mutex
	roomObservers map[string][]chan struct{}
}

// NewPartialStateTracker creates a new PartialStateTracker.
func NewPartialStateTracker() *PartialStateTracker {
	return &PartialStateTracker{
		roomObservers: make(map[string][]chan struct{}),
	}
}

// Observe registers a waiter for the room. The returned channel is closed
// once the room has full state; the returned function must be called when
// the caller stops waiting.
func (t *PartialStateTracker) Observe(roomID string) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	t.mu.Lock()
	t.roomObservers[roomID] = append(t.roomObservers[roomID], ch)
	t.mu.Unlock()
	return ch, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		observers := t.roomObservers[roomID]
		for i, observer := range observers {
			if observer == ch {
				t.roomObservers[roomID] = append(observers[:i], observers[i+1:]...)
				break
			}
		}
		if len(t.roomObservers[roomID]) == 0 {
			delete(t.roomObservers, roomID)
		}
	}
}

// AwaitFullState waits until the room has full state, as reported by
// isPartial, or the context ends. Registering before asking isPartial means
// that a resync finishing in between is not missed.
func (t *PartialStateTracker) AwaitFullState(ctx context.Context, roomID string, isPartial func() (bool, error)) error {
	ch, stop := t.Observe(roomID)
	defer stop()
	partial, err := isPartial()
	if err != nil || !partial {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}

// NotifyUnPartialStated wakes every waiter of the room.
func (t *PartialStateTracker) NotifyUnPartialStated(roomID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ch := range t.roomObservers[roomID] {
		close(ch)
	}
	delete(t.roomObservers, roomID)
}

// PendingRoomCount returns the number of rooms with waiters.
func (t *PartialStateTracker) PendingRoomCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.roomObservers)
}

// HasObservers reports whether anything waits for the room.
func (t *PartialStateTracker) HasObservers(roomID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.roomObservers[roomID]) > 0
}
