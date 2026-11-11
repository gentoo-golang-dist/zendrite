// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package notifier

import "sync"

// unPartialStated lets requests wait until the sync API has stored the full
// state of a room that was joined with partial state (MSC3902).
type unPartialStated struct {
	mu        sync.Mutex
	observers map[string][]chan struct{}
}

// ObserveUnPartialStated returns a channel that is closed once the sync API
// has stored the full state of the room, and a function to call when the
// caller stops waiting.
func (n *Notifier) ObserveUnPartialStated(roomID string) (<-chan struct{}, func()) {
	t := &n.unPartialStated
	ch := make(chan struct{})
	t.mu.Lock()
	if t.observers == nil {
		t.observers = map[string][]chan struct{}{}
	}
	t.observers[roomID] = append(t.observers[roomID], ch)
	t.mu.Unlock()
	return ch, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		observers := t.observers[roomID]
		for i, observer := range observers {
			if observer == ch {
				t.observers[roomID] = append(observers[:i], observers[i+1:]...)
				break
			}
		}
		if len(t.observers[roomID]) == 0 {
			delete(t.observers, roomID)
		}
	}
}

// OnUnPartialStated wakes the requests waiting for the full state of the room.
func (n *Notifier) OnUnPartialStated(roomID string) {
	t := &n.unPartialStated
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ch := range t.observers[roomID] {
		close(ch)
	}
	delete(t.observers, roomID)
}
