// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// MSC4293: a kick or ban with redact_events redacts the events the target
// sent since their previous membership, and those that arrive late, with
// the kick or ban as their redacted_because, if its sender may redact.
func TestRedactOnKickOrBan(t *testing.T) {
	alice, bob, charlie := test.NewUser(t), test.NewUser(t), test.NewUser(t)
	ctx := context.Background()
	member := func(membership string, extra ...map[string]any) map[string]any {
		content := map[string]any{"membership": membership}
		for _, e := range extra {
			for k, v := range e {
				content[k] = v
			}
		}
		return content
	}
	redact := map[string]any{"org.matrix.msc4293.redact_events": true}

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
		defer closeRig()
		cfg.MSCs.MSCs = []string{"msc4293"}
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)

		events := func(roomID string, ids ...string) map[string]*types.HeaderedEvent {
			t.Helper()
			var res api.QueryEventsByIDResponse
			require.NoError(t, rsAPI.QueryEventsByID(ctx, &api.QueryEventsByIDRequest{RoomID: roomID, EventIDs: ids}, &res))
			byID := make(map[string]*types.HeaderedEvent, len(res.Events))
			for _, ev := range res.Events {
				byID[ev.EventID()] = ev
			}
			return byID
		}
		// redactedBy returns the ID of the event that redacted ev, after
		// checking that redacted_because holds a ban as the event itself.
		redactedBy := func(ev *types.HeaderedEvent) string {
			unsigned := gjson.ParseBytes(ev.Unsigned())
			if !unsigned.Get("redacted_because").Exists() {
				return ""
			}
			require.Equal(t, spec.MRoomMember, unsigned.Get("redacted_because.type").Str)
			require.Equal(t, spec.Ban, unsigned.Get("redacted_because.content.membership").Str)
			return unsigned.Get("redacted_by").Str
		}

		t.Run("ban", func(t *testing.T) {
			room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
			room.CreateAndInsert(t, bob, spec.MRoomMember, member(spec.Join), test.WithStateKey(bob.ID))
			before := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "before"})
			room.CreateAndInsert(t, bob, spec.MRoomMember, member(spec.Leave), test.WithStateKey(bob.ID))
			room.CreateAndInsert(t, bob, spec.MRoomMember, member(spec.Join), test.WithStateKey(bob.ID))
			spam := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "spam"})
			other := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "mine"})
			// Bob's server sends this before it learns of the ban.
			late := room.CreateEvent(t, bob, "m.room.message", map[string]any{"body": "late"})
			ban := room.CreateAndInsert(t, alice, spec.MRoomMember, member(spec.Ban, redact), test.WithStateKey(bob.ID))
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
			// The late event is soft-failed: it was allowed at its own state.
			_ = api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{late}, "test", "test", "test", nil, false)

			got := events(room.ID, before.EventID(), spam.EventID(), other.EventID(), late.EventID())
			require.Equal(t, ban.EventID(), redactedBy(got[spam.EventID()]))
			require.False(t, gjson.GetBytes(got[spam.EventID()].Content(), "body").Exists())
			require.Empty(t, redactedBy(got[before.EventID()]), "events before the previous membership are kept")
			require.Empty(t, redactedBy(got[other.EventID()]), "other users' events are kept")
			require.Contains(t, got, late.EventID())
			require.Equal(t, ban.EventID(), redactedBy(got[late.EventID()]), "an event that arrives late is redacted too")
		})

		t.Run("profile changes and kicks", func(t *testing.T) {
			room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
			room.CreateAndInsert(t, bob, spec.MRoomMember, member(spec.Join), test.WithStateKey(bob.ID))
			kept := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "before the kick"})
			room.CreateAndInsert(t, alice, spec.MRoomMember, member(spec.Leave), test.WithStateKey(bob.ID))
			room.CreateAndInsert(t, bob, spec.MRoomMember, member(spec.Join), test.WithStateKey(bob.ID))
			first := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "spam"})
			// A profile change does not start a new stay.
			room.CreateAndInsert(t, bob, spec.MRoomMember, member(spec.Join, map[string]any{"displayname": "Spammer"}), test.WithStateKey(bob.ID))
			second := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "more spam"})
			ban := room.CreateAndInsert(t, alice, spec.MRoomMember, member(spec.Ban, redact), test.WithStateKey(bob.ID))
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))

			got := events(room.ID, kept.EventID(), first.EventID(), second.EventID())
			require.Equal(t, ban.EventID(), redactedBy(got[first.EventID()]))
			require.Equal(t, ban.EventID(), redactedBy(got[second.EventID()]))
			require.Empty(t, redactedBy(got[kept.EventID()]), "a kick ends the previous stay")
		})

		t.Run("not for an unban", func(t *testing.T) {
			room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
			room.CreateAndInsert(t, bob, spec.MRoomMember, member(spec.Join), test.WithStateKey(bob.ID))
			message := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "hi"})
			room.CreateAndInsert(t, alice, spec.MRoomMember, member(spec.Ban), test.WithStateKey(bob.ID))
			room.CreateAndInsert(t, alice, spec.MRoomMember, member(spec.Leave, redact), test.WithStateKey(bob.ID))
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
			require.Empty(t, redactedBy(events(room.ID, message.EventID())[message.EventID()]))
		})

		t.Run("not without power to redact", func(t *testing.T) {
			room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
			room.CreateAndInsert(t, charlie, spec.MRoomMember, member(spec.Join), test.WithStateKey(charlie.ID))
			room.CreateAndInsert(t, alice, spec.MRoomPowerLevels, map[string]any{
				"users": map[string]any{alice.ID: 100, charlie.ID: 50}, "ban": 50, "kick": 50, "redact": 100,
			}, test.WithStateKey(""))
			room.CreateAndInsert(t, bob, spec.MRoomMember, member(spec.Join), test.WithStateKey(bob.ID))
			spam := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "spam"})
			room.CreateAndInsert(t, charlie, spec.MRoomMember, member(spec.Leave, redact), test.WithStateKey(bob.ID))
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
			require.Empty(t, redactedBy(events(room.ID, spam.EventID())[spam.EventID()]))
		})

		t.Run("not for a voluntary leave", func(t *testing.T) {
			room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
			room.CreateAndInsert(t, bob, spec.MRoomMember, member(spec.Join), test.WithStateKey(bob.ID))
			message := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "bye"})
			room.CreateAndInsert(t, bob, spec.MRoomMember, member(spec.Leave, redact), test.WithStateKey(bob.ID))
			require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
			require.Empty(t, redactedBy(events(room.ID, message.EventID())[message.EventID()]))
		})
	})
}
