// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package syncapi

import (
	"context"
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
	rsstorage "codefloe.com/pat-s/zendrite/roomserver/storage"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// TestUnredactedContent checks MSC2815: room moderators and server admins
// can fetch the content of a redacted event with
// fi.mau.msc2815.include_unredacted_content, while everyone else, and every
// other endpoint, still gets the redacted event.
func TestUnredactedContent(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)   // room creator, power level 100
		bob := test.NewUser(t)     // member, power level 0
		charlie := test.NewUser(t) // member, power level 0, server admin
		dave := test.NewUser(t)    // not a member, server admin
		aliceDev := api.Device{ID: "ALICEID", UserID: alice.ID, AccessToken: "ALICE_MSC2815_TOKEN", AccountType: api.AccountTypeUser}
		bobDev := api.Device{ID: "BOBID", UserID: bob.ID, AccessToken: "BOB_MSC2815_TOKEN", AccountType: api.AccountTypeUser}
		charlieDev := api.Device{ID: "CHARLIEID", UserID: charlie.ID, AccessToken: "CHARLIE_MSC2815_TOKEN", AccountType: api.AccountTypeAdmin}
		daveDev := api.Device{ID: "DAVEID", UserID: dave.ID, AccessToken: "DAVE_MSC2815_TOKEN", AccountType: api.AccountTypeAdmin}

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
		AddPublicRoutes(processCtx, routers, cfg, cm, &natsInstance, &syncUserAPI{accounts: []api.Device{aliceDev, bobDev, charlieDev, daveDev}}, rsAPI, caches, caching.DisableMetrics, nil, nil)

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat), test.RoomHistoryVisibility(gomatrixserverlib.HistoryVisibilityShared))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		room.CreateAndInsert(t, charlie, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(charlie.ID))
		purged := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "old spam"})
		room.CreateAndInsert(t, alice, spec.MRoomRedaction, map[string]any{}, test.WithRedacts(purged.EventID()))
		visible := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "hello"})
		require.NoError(t, rsapi.SendEvents(context.Background(), rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))

		// Delete the content of the first redacted message, as the expiry job
		// does once the retention period has passed.
		rsDB, err := rsstorage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches)
		require.NoError(t, err)
		rsDB.SetRedactedContentRetention(0)
		removed, err := rsDB.ExpireRedactedContent(context.Background())
		require.NoError(t, err)
		require.Equal(t, int64(1), removed)

		kept := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "spam"})
		redaction := room.CreateAndInsert(t, bob, spec.MRoomRedaction, map[string]any{}, test.WithRedacts(kept.EventID()))
		require.NoError(t, rsapi.SendEvents(context.Background(), rsAPI, rsapi.KindNew, []*rstypes.HeaderedEvent{kept, redaction}, "test", "test", "test", nil, false))

		syncUntil(t, routers, aliceDev.AccessToken, false, func(syncBody string) bool {
			return gjson.Get(syncBody, fmt.Sprintf(`rooms.join.%s.timeline.events.#(event_id=="%s")`, room.ID, redaction.EventID())).Exists()
		})

		get := func(dev api.Device, eventID string, include string) *httptest.ResponseRecorder {
			params := map[string]string{"access_token": dev.AccessToken}
			if include != "" {
				params["fi.mau.msc2815.include_unredacted_content"] = include
			}
			w := httptest.NewRecorder()
			routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, fmt.Sprintf("/_matrix/client/v3/rooms/%s/event/%s", room.ID, eventID), test.WithQueryParams(params)))
			return w
		}

		// Without the parameter, and with it set to false, the event is
		// redacted for everyone.
		for _, include := range []string{"", "false"} {
			w := get(aliceDev, kept.EventID(), include)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.False(t, gjson.Get(w.Body.String(), "content.body").Exists(), w.Body.String())
			assert.Equal(t, redaction.EventID(), gjson.Get(w.Body.String(), "unsigned.redacted_because.event_id").Str)
		}

		// A moderator gets the content, with redacted_because.
		w := get(aliceDev, kept.EventID(), "true")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "spam", gjson.Get(w.Body.String(), "content.body").Str)
		assert.Equal(t, kept.EventID(), gjson.Get(w.Body.String(), "event_id").Str)
		assert.Equal(t, redaction.EventID(), gjson.Get(w.Body.String(), "unsigned.redacted_because.event_id").Str)

		// A member below the redact level may not ask, even for their own
		// event.
		w = get(bobDev, kept.EventID(), "true")
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		assert.Equal(t, "M_FORBIDDEN", gjson.Get(w.Body.String(), "errcode").Str)

		// Server admins may, if they can see the event.
		w = get(charlieDev, kept.EventID(), "true")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "spam", gjson.Get(w.Body.String(), "content.body").Str)
		w = get(daveDev, kept.EventID(), "true")
		require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		assert.Equal(t, "M_NOT_FOUND", gjson.Get(w.Body.String(), "errcode").Str)

		// Events that are not redacted are returned as they are.
		w = get(aliceDev, visible.EventID(), "true")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "hello", gjson.Get(w.Body.String(), "content.body").Str)

		// Content that is no longer kept.
		w = get(aliceDev, purged.EventID(), "true")
		require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		assert.Equal(t, "FI.MAU.MSC2815_UNREDACTED_CONTENT_DELETED", gjson.Get(w.Body.String(), "errcode").Str)
		assert.Equal(t, config.DefaultRedactionRetentionPeriod.Milliseconds(), gjson.Get(w.Body.String(), "fi\\.mau\\.msc2815\\.content_keep_ms").Int())

		// The parameter only takes true or false.
		w = get(aliceDev, kept.EventID(), "yes")
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Equal(t, "M_INVALID_PARAM", gjson.Get(w.Body.String(), "errcode").Str)

		// Creators may ask in room versions where they have unlimited power
		// and are not listed in the power levels, and in rooms without power
		// levels.
		for _, mod := range []test.RoomModifier{test.RoomVersion(gomatrixserverlib.RoomVersionV12), test.WithoutPowerLevels()} {
			otherRoom := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat), mod)
			message := otherRoom.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "typo"})
			redaction := otherRoom.CreateAndInsert(t, alice, spec.MRoomRedaction, map[string]any{"redacts": message.EventID()}, test.WithRedacts(message.EventID()))
			require.NoError(t, rsapi.SendEvents(context.Background(), rsAPI, rsapi.KindNew, otherRoom.Events(), "test", "test", "test", nil, false))
			syncUntil(t, routers, aliceDev.AccessToken, false, func(syncBody string) bool {
				return gjson.Get(syncBody, fmt.Sprintf(`rooms.join.%s.timeline.events.#(event_id=="%s")`, otherRoom.ID, redaction.EventID())).Exists()
			})
			w = httptest.NewRecorder()
			routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, fmt.Sprintf("/_matrix/client/v3/rooms/%s/event/%s", otherRoom.ID, message.EventID()), test.WithQueryParams(map[string]string{
				"access_token": aliceDev.AccessToken,
				"fi.mau.msc2815.include_unredacted_content": "true",
			})))
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, "typo", gjson.Get(w.Body.String(), "content.body").Str)
		}

		// /messages and /sync still return the redacted event.
		w = httptest.NewRecorder()
		routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages", room.ID), test.WithQueryParams(map[string]string{
			"access_token": aliceDev.AccessToken,
			"dir":          "b",
			"limit":        "50",
		})))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		chunk := gjson.Get(w.Body.String(), "chunk")
		for _, eventID := range []string{kept.EventID(), purged.EventID()} {
			ev := chunk.Get(fmt.Sprintf(`#(event_id=="%s")`, eventID))
			require.True(t, ev.Exists(), "missing %s", eventID)
			assert.False(t, ev.Get("content.body").Exists(), ev.Raw)
		}
		w = httptest.NewRecorder()
		routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, "/_matrix/client/v3/sync", test.WithQueryParams(map[string]string{
			"access_token": aliceDev.AccessToken,
			"filter":       `{"room":{"timeline":{"limit":50}}}`,
		})))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		timeline := gjson.Get(w.Body.String(), fmt.Sprintf("rooms.join.%s.timeline.events", room.ID))
		ev := timeline.Get(fmt.Sprintf(`#(event_id=="%s")`, kept.EventID()))
		require.True(t, ev.Exists(), timeline.Raw)
		assert.False(t, ev.Get("content.body").Exists(), ev.Raw)
	})
}
