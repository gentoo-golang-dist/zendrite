// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package syncapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// TestMessagesWithoutFrom checks that "from" is optional on /messages
// (MSC3567): going forward, the first page starts at the beginning of the
// room, and going backward, at its end.
func TestMessagesWithoutFrom(t *testing.T) {
	test.WithAllDatabases(t, testMessagesWithoutFrom)
}

func testMessagesWithoutFrom(t *testing.T, dbType test.DBType) {
	alice := test.NewUser(t)
	aliceDev := userapi.Device{ID: "ALICEID", UserID: alice.ID, AccessToken: "ALICE_BEARER_TOKEN", AccountType: userapi.AccountTypeUser}

	cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
	defer closeRig()
	cfg.ClientAPI.RateLimiting = config.RateLimiting{Enabled: false}
	routers := httputil.NewRouters()
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	natsInstance := jetstream.NATSInstance{}
	jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
	defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	AddPublicRoutes(processCtx, routers, cfg, cm, &natsInstance, accountDataUserAPI{&syncUserAPI{accounts: []userapi.Device{aliceDev}}}, rsAPI, caches, caching.DisableMetrics, nil, nil)

	r := &stateAfterRig{t: t, routers: routers, rsAPI: rsAPI, alice: alice, tokens: map[*test.User]string{alice: aliceDev.AccessToken}}

	room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
	for i := range 5 {
		room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": fmt.Sprintf("message %d", i)})
	}
	events := room.Events()
	r.send(events...)
	last := events[len(events)-1].EventID()
	r.syncUntil(alice, nil, func(res string) bool {
		return slices.Contains(eventIDs(res, "rooms.join."+gjson.Escape(room.ID), "timeline"), last)
	})

	ids := func(events ...*rstypes.HeaderedEvent) []string {
		out := make([]string, len(events))
		for i, ev := range events {
			out[i] = ev.EventID()
		}
		return out
	}
	messages := func(params map[string]string) ([]string, string) {
		t.Helper()
		query := map[string]string{"access_token": aliceDev.AccessToken, "limit": "3"}
		for k, v := range params {
			query[k] = v
		}
		w := httptest.NewRecorder()
		routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages", room.ID), test.WithQueryParams(query)))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var got []string
		for _, ev := range gjson.GetBytes(w.Body.Bytes(), "chunk").Array() {
			got = append(got, ev.Get("event_id").Str)
		}
		return got, gjson.GetBytes(w.Body.Bytes(), "end").Str
	}

	t.Run("forward starts at the beginning of the room", func(t *testing.T) {
		got, end := messages(map[string]string{"dir": "f"})
		assert.Equal(t, ids(events[:3]...), got)
		require.NotEmpty(t, end)
		got, _ = messages(map[string]string{"dir": "f", "from": end})
		assert.Equal(t, ids(events[3:6]...), got)
	})

	t.Run("backward starts at the end of the room", func(t *testing.T) {
		got, _ := messages(map[string]string{"dir": "b"})
		assert.Equal(t, ids(test.Reversed(events[len(events)-3:])...), got)
	})
}
