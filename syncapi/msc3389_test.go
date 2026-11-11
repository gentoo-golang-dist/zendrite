// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package syncapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// TestMSC3389RelationRedactions checks that in room version
// org.matrix.msc3389.10 a redacted event keeps m.relates_to.rel_type and
// m.relates_to.event_id everywhere the redacted event is served, while room
// moderators still get the original content (MSC2815), and that room
// version 10 strips the relation as before.
func TestMSC3389RelationRedactions(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		for _, tc := range []struct {
			version      gomatrixserverlib.RoomVersion
			keepRelation bool
		}{
			{gomatrixserverlib.RoomVersionMSC3389, true},
			{gomatrixserverlib.RoomVersionV10, false},
		} {
			t.Run(string(tc.version), func(t *testing.T) {
				testMSC3389RelationRedactions(t, dbType, tc.version, tc.keepRelation)
			})
		}
	})
}

func testMSC3389RelationRedactions(t *testing.T, dbType test.DBType, roomVersion gomatrixserverlib.RoomVersion, keepRelation bool) {
	alice := test.NewUser(t)
	aliceDev := api.Device{ID: "ALICEID", UserID: alice.ID, AccessToken: "ALICE_MSC3389_TOKEN", AccountType: api.AccountTypeUser}

	cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
	defer closeDB()
	cfg.ClientAPI.RateLimiting = config.RateLimiting{Enabled: false}
	routers := httputil.NewRouters()
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	natsInstance := jetstream.NATSInstance{}
	jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
	defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	AddPublicRoutes(processCtx, routers, cfg, cm, &natsInstance, &syncUserAPI{accounts: []api.Device{aliceDev}}, rsAPI, caches, caching.DisableMetrics, nil, nil)

	ctx := context.Background()
	room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat), test.RoomVersion(roomVersion))
	root := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "root"})
	reaction := room.CreateAndInsert(t, alice, "m.reaction", map[string]any{
		"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": root.EventID(), "key": "👍"},
	})
	reply := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "reply", "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root.EventID()},
	})
	room.CreateAndInsert(t, alice, spec.MRoomRedaction, map[string]any{}, test.WithRedacts(reaction.EventID()))
	replyRedaction := room.CreateAndInsert(t, alice, spec.MRoomRedaction, map[string]any{}, test.WithRedacts(reply.EventID()))
	require.NoError(t, rsapi.SendEvents(ctx, rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))

	// A redaction that arrives before the event it redacts: the roomserver
	// redacts the event as it stores it.
	beforeLate := replyRedaction.EventID()
	late := room.CreateAndInsert(t, alice, "m.reaction", map[string]any{
		"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": root.EventID(), "key": "👎"},
	}, test.WithPrevEvents(beforeLate))
	early := room.CreateAndInsert(t, alice, spec.MRoomRedaction, map[string]any{}, test.WithRedacts(late.EventID()), test.WithPrevEvents(beforeLate))
	require.NoError(t, rsapi.SendEvents(ctx, rsAPI, rsapi.KindNew, []*rstypes.HeaderedEvent{early}, "test", "test", "test", nil, false))
	require.NoError(t, rsapi.SendEvents(ctx, rsAPI, rsapi.KindNew, []*rstypes.HeaderedEvent{late}, "test", "test", "test", nil, false))

	syncUntil(t, routers, aliceDev.AccessToken, false, func(syncBody string) bool {
		events := gjson.Get(syncBody, fmt.Sprintf(`rooms.join.%s.timeline.events`, gjson.Escape(room.ID)))
		return events.Get(fmt.Sprintf(`#(event_id=="%s")`, late.EventID())).Exists() &&
			events.Get(fmt.Sprintf(`#(event_id=="%s")`, replyRedaction.EventID())).Exists()
	})

	request := func(t *testing.T, path string, query map[string]string) gjson.Result {
		t.Helper()
		params := map[string]string{"access_token": aliceDev.AccessToken}
		for k, v := range query {
			params[k] = v
		}
		w := httptest.NewRecorder()
		routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, path, test.WithQueryParams(params)))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		return gjson.ParseBytes(w.Body.Bytes())
	}
	byID := func(events gjson.Result, eventID string) gjson.Result {
		return events.Get(fmt.Sprintf(`#(event_id=="%s")`, eventID))
	}
	// wantContent is the redacted content of an event relating to root.
	wantContent := func(relType string) string {
		if !keepRelation {
			return `{}`
		}
		return fmt.Sprintf(`{"m.relates_to":{"event_id":%q,"rel_type":%q}}`, root.EventID(), relType)
	}
	requireRedacted := func(t *testing.T, event gjson.Result, relType string) {
		t.Helper()
		require.True(t, event.Exists(), "event missing")
		assert.JSONEq(t, wantContent(relType), event.Get("content").Raw, event.Raw)
		assert.True(t, event.Get("unsigned.redacted_because").Exists(), event.Raw)
	}
	roomPath := "/_matrix/client/v3/rooms/" + room.ID

	t.Run("event", func(t *testing.T) {
		requireRedacted(t, request(t, roomPath+"/event/"+reaction.EventID(), nil), "m.annotation")
		requireRedacted(t, request(t, roomPath+"/event/"+reply.EventID(), nil), "m.thread")
		requireRedacted(t, request(t, roomPath+"/event/"+late.EventID(), nil), "m.annotation")
	})
	t.Run("messages", func(t *testing.T) {
		chunk := request(t, roomPath+"/messages", map[string]string{"dir": "b", "limit": "50"}).Get("chunk")
		requireRedacted(t, byID(chunk, reaction.EventID()), "m.annotation")
		requireRedacted(t, byID(chunk, reply.EventID()), "m.thread")
		requireRedacted(t, byID(chunk, late.EventID()), "m.annotation")
	})
	t.Run("sync", func(t *testing.T) {
		filter, err := json.Marshal(map[string]any{"room": map[string]any{"timeline": map[string]any{"limit": 50}}})
		require.NoError(t, err)
		events := request(t, "/_matrix/client/v3/sync", map[string]string{"filter": string(filter)}).Get("rooms.join." + gjson.Escape(room.ID) + ".timeline.events")
		requireRedacted(t, byID(events, reaction.EventID()), "m.annotation")
		requireRedacted(t, byID(events, reply.EventID()), "m.thread")
		requireRedacted(t, byID(events, late.EventID()), "m.annotation")
	})
	t.Run("moderator view", func(t *testing.T) {
		// MSC2815 still serves the whole original content.
		for _, ev := range []*rstypes.HeaderedEvent{reaction, late} {
			event := request(t, roomPath+"/event/"+ev.EventID(), map[string]string{"fi.mau.msc2815.include_unredacted_content": "true"})
			assert.JSONEq(t, string(ev.Content()), event.Get("content").Raw)
		}
	})
	t.Run("relations", func(t *testing.T) {
		// Redacted events are no longer listed as relations of the root,
		// even though they keep their relation.
		chunk := request(t, "/_matrix/client/v1/rooms/"+room.ID+"/relations/"+root.EventID(), nil).Get("chunk")
		assert.Empty(t, chunk.Array(), chunk.Raw)
		root := request(t, roomPath+"/event/"+root.EventID(), nil)
		assert.False(t, root.Get(`unsigned.m\.relations.m\.thread`).Exists(), root.Raw)
	})
	t.Run("annotations", func(t *testing.T) {
		// The redacted reactions do not count as sent, so they can be sent again.
		for _, key := range []string{"👍", "👎"} {
			exists, err := rsAPI.QueryAnnotationExists(ctx, room.ID, root.EventID(), spec.SenderID(alice.ID), "m.reaction", key, nil)
			require.NoError(t, err)
			assert.False(t, exists, key)
		}
	})
}
