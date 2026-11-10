// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/storage"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// redactedContentHarness starts a roomserver with the given retention period
// and returns it with a second handle on its database, whose retention the
// test sets itself.
func redactedContentHarness(t *testing.T, dbType test.DBType, retention time.Duration) (api.RoomserverInternalAPI, storage.Database) {
	t.Helper()
	cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
	t.Cleanup(closeDB)
	cfg.RoomServer.RedactionRetentionPeriod = retention
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(8*1024*1024, time.Hour, caching.DisableMetrics)
	natsInstance := jetstream.NATSInstance{}
	jsCtx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
	t.Cleanup(func() { jetstream.DeleteAllStreams(jsCtx, &cfg.Global.JetStream) })
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches)
	require.NoError(t, err)
	return rsAPI, db
}

// TestRedactedContentRetention checks that the roomserver keeps the content
// of redacted events for room moderators (MSC2815) for the retention period
// only, while the event itself stays redacted.
func TestRedactedContentRetention(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		bob := test.NewUser(t)
		rsAPI, db := redactedContentHarness(t, dbType, config.DefaultRedactionRetentionPeriod)
		ctx := context.Background()

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		message := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "spam"})
		redaction := room.CreateAndInsert(t, alice, spec.MRoomRedaction, map[string]any{}, test.WithRedacts(message.EventID()))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
		roomID, err := spec.NewRoomID(room.ID)
		require.NoError(t, err)

		// Everything else gets the redacted event.
		res := api.QueryEventsByIDResponse{}
		require.NoError(t, rsAPI.QueryEventsByID(ctx, &api.QueryEventsByIDRequest{RoomID: room.ID, EventIDs: []string{message.EventID()}}, &res))
		require.Len(t, res.Events, 1)
		assert.True(t, res.Events[0].Redacted())
		assert.False(t, gjson.GetBytes(res.Events[0].Content(), "body").Exists())

		original, retention, err := rsAPI.QueryUnredactedEvent(ctx, *roomID, message.EventID())
		require.NoError(t, err)
		assert.Equal(t, config.DefaultRedactionRetentionPeriod, retention)
		require.NotNil(t, original)
		assert.Equal(t, message.EventID(), original.EventID())
		assert.Equal(t, "spam", gjson.GetBytes(original.Content(), "body").Str)

		// Only for the room the event is in, and only for redacted events.
		otherRoom, err := spec.NewRoomID("!other:test")
		require.NoError(t, err)
		original, _, err = rsAPI.QueryUnredactedEvent(ctx, *otherRoom, message.EventID())
		require.NoError(t, err)
		assert.Nil(t, original)
		original, _, err = rsAPI.QueryUnredactedEvent(ctx, *roomID, redaction.EventID())
		require.NoError(t, err)
		assert.Nil(t, original)

		roomInfo, err := db.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		require.NotNil(t, roomInfo)

		// Kept until the room is purged with a negative retention period.
		db.SetRedactedContentRetention(-1)
		removed, err := db.ExpireRedactedContent(ctx)
		require.NoError(t, err)
		assert.Zero(t, removed)
		content, err := db.RedactedEventContent(ctx, roomInfo.RoomNID, message.EventID())
		require.NoError(t, err)
		assert.NotNil(t, content)

		// Once the retention period has passed the content is no longer
		// served, then deleted.
		db.SetRedactedContentRetention(time.Millisecond)
		time.Sleep(5 * time.Millisecond)
		content, err = db.RedactedEventContent(ctx, roomInfo.RoomNID, message.EventID())
		require.NoError(t, err)
		assert.Nil(t, content)
		removed, err = db.ExpireRedactedContent(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(1), removed)
		db.SetRedactedContentRetention(-1)
		content, err = db.RedactedEventContent(ctx, roomInfo.RoomNID, message.EventID())
		require.NoError(t, err)
		assert.Nil(t, content)

		// Purging the room deletes the content kept for it.
		second := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "more spam"})
		secondRedaction := room.CreateAndInsert(t, alice, spec.MRoomRedaction, map[string]any{}, test.WithRedacts(second.EventID()))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{second, secondRedaction}, "test", "test", "test", nil, false))
		content, err = db.RedactedEventContent(ctx, roomInfo.RoomNID, second.EventID())
		require.NoError(t, err)
		assert.Equal(t, "more spam", gjson.GetBytes(content, "content.body").Str)
		require.NoError(t, db.PurgeRoom(ctx, room.ID))
		db.SetRedactedContentRetention(0)
		removed, err = db.ExpireRedactedContent(ctx)
		require.NoError(t, err)
		assert.Zero(t, removed, "the purge left content behind")
	})
}

// TestRedactedContentNotKept checks that a retention period of zero keeps no
// content of redacted events.
func TestRedactedContentNotKept(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		rsAPI, db := redactedContentHarness(t, dbType, 0)
		ctx := context.Background()

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		message := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "oops"})
		room.CreateAndInsert(t, alice, spec.MRoomRedaction, map[string]any{}, test.WithRedacts(message.EventID()))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
		roomID, err := spec.NewRoomID(room.ID)
		require.NoError(t, err)

		original, retention, err := rsAPI.QueryUnredactedEvent(ctx, *roomID, message.EventID())
		require.NoError(t, err)
		assert.Nil(t, original)
		assert.Zero(t, retention)

		db.SetRedactedContentRetention(-1)
		roomInfo, err := db.RoomInfo(ctx, room.ID)
		require.NoError(t, err)
		content, err := db.RedactedEventContent(ctx, roomInfo.RoomNID, message.EventID())
		require.NoError(t, err)
		assert.Nil(t, content, "content was stored")
	})
}
