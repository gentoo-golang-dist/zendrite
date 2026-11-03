// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"context"
	"errors"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
)

// excludeRecordingDatabase records whether each event is written excluded
// from sync, then fails the write after ending the context, so that the
// consumer returns instead of carrying on.
type excludeRecordingDatabase struct {
	storage.Database
	cancel   context.CancelCauseFunc
	excluded map[string]bool
}

var errStopAfterWrite = errors.New("stop after the write")

func (d *excludeRecordingDatabase) WriteEvent(
	_ context.Context, ev *rstypes.HeaderedEvent, _ []*rstypes.HeaderedEvent, _, _ []string,
	_ *api.TransactionID, excludeFromSync bool, _ gomatrixserverlib.HistoryVisibility,
) (types.StreamPosition, error) {
	d.excluded[ev.EventID()] = excludeFromSync
	d.cancel(errStopAfterWrite)
	return 0, errStopAfterWrite
}

// Events a repair announces long after they were sent must be kept from the
// timelines of /sync. Written at a new stream position, they would otherwise
// become the room's latest events, and a client paginating back from them
// would skip every event sent since.
func TestRepairedEventsAreExcludedFromSync(t *testing.T) {
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	message := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "hello"})
	member := room.CreateAndInsert(t, alice, "m.room.member", map[string]any{"membership": "join", "displayname": "Alice"}, test.WithStateKey(alice.ID))

	write := func(t *testing.T, handle func(*OutputRoomEventConsumer, context.Context) error) map[string]bool {
		t.Helper()
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		db := &excludeRecordingDatabase{cancel: cancel, excluded: map[string]bool{}}
		s := &OutputRoomEventConsumer{db: db, rsAPI: senderResolver{user: alice}}
		require.ErrorIs(t, handle(s, ctx), errStopAfterWrite)
		return db.excluded
	}

	for name, tc := range map[string]struct {
		handle func(*OutputRoomEventConsumer, context.Context) error
		event  *rstypes.HeaderedEvent
		want   bool
	}{
		"old message": {
			handle: func(s *OutputRoomEventConsumer, ctx context.Context) error {
				return s.onOldRoomEvent(ctx, api.OutputOldRoomEvent{Event: message})
			},
			event: message,
		},
		"old message from a repair": {
			handle: func(s *OutputRoomEventConsumer, ctx context.Context) error {
				return s.onOldRoomEvent(ctx, api.OutputOldRoomEvent{Event: message, ExcludeFromSync: true})
			},
			event: message,
			want:  true,
		},
		"old state event": {
			handle: func(s *OutputRoomEventConsumer, ctx context.Context) error {
				return s.onOldRoomEvent(ctx, api.OutputOldRoomEvent{Event: member})
			},
			event: member,
			want:  true,
		},
		// The consumer writes new state events the same way, after
		// updating the current state, which this database cannot do.
		"new event": {
			handle: func(s *OutputRoomEventConsumer, ctx context.Context) error {
				return s.onNewRoomEvent(ctx, api.OutputNewRoomEvent{Event: message})
			},
			event: message,
		},
		"new event from a repair": {
			handle: func(s *OutputRoomEventConsumer, ctx context.Context) error {
				return s.onNewRoomEvent(ctx, api.OutputNewRoomEvent{Event: message, ExcludeFromSync: true})
			},
			event: message,
			want:  true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			excluded := write(t, tc.handle)
			require.Contains(t, excluded, tc.event.EventID())
			require.Equal(t, tc.want, excluded[tc.event.EventID()])
		})
	}
}
