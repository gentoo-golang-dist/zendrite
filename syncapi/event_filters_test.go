// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package syncapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
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

// TestRelationAndLabelEventFilters checks the relation type filters of
// MSC3874 and the label filters of MSC2326 on /messages, /sync and /context,
// with the unstable names Synapse uses. The /messages cases follow
// Complement's msc3874 TestFilterMessagesByRelType.
func TestRelationAndLabelEventFilters(t *testing.T) {
	test.WithAllDatabases(t, testRelationAndLabelEventFilters)
}

func testRelationAndLabelEventFilters(t *testing.T, dbType test.DBType) {
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

	r := &stateAfterRig{
		t: t, routers: routers, rsAPI: rsAPI, alice: alice,
		tokens: map[*test.User]string{alice: aliceDev.AccessToken},
	}

	room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
	path := "rooms.join." + gjson.Escape(room.ID)
	created := room.Events()
	r.send(created...)
	res := r.syncUntil(alice, nil, func(res string) bool {
		return slices.Contains(eventIDs(res, path, "timeline"), created[len(created)-1].EventID())
	})
	since := gjson.Get(res, "next_batch").Str

	root := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "Message without a relation", "org.matrix.labels": []string{"#fun"},
	})
	thread := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "Threaded Reply",
		"m.relates_to": map[string]any{"event_id": root.EventID(), "rel_type": "m.thread"},
	})
	reference := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "Reference Reply", "org.matrix.labels": []string{"#fun", "#work"},
		"m.relates_to": map[string]any{"event_id": root.EventID(), "rel_type": "m.reference"},
	})
	r.send(root, thread, reference)
	res = r.syncUntil(alice, map[string]string{"since": since}, func(res string) bool {
		return hasMessage(res, path, "Reference Reply")
	})
	end := gjson.Get(res, "next_batch").Str

	ids := func(events ...*rstypes.HeaderedEvent) []string {
		out := make([]string, len(events))
		for i, ev := range events {
			out[i] = ev.EventID()
		}
		return out
	}
	chunk := func(body []byte, field string) []string {
		var got []string
		for _, ev := range gjson.GetBytes(body, field).Array() {
			got = append(got, ev.Get("event_id").Str)
		}
		return got
	}
	get := func(path string, query map[string]string) []byte {
		t.Helper()
		query["access_token"] = aliceDev.AccessToken
		w := httptest.NewRecorder()
		routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, path, test.WithQueryParams(query)))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		return w.Body.Bytes()
	}

	for _, tc := range []struct {
		filter string
		want   []*rstypes.HeaderedEvent
	}{
		{`{"org.matrix.msc3874.rel_types": ["m.thread"]}`, []*rstypes.HeaderedEvent{thread}},
		{`{"org.matrix.msc3874.rel_types": ["m.reference"]}`, []*rstypes.HeaderedEvent{reference}},
		{`{"org.matrix.msc3874.rel_types": ["m.thread", "m.reference"]}`, []*rstypes.HeaderedEvent{thread, reference}},
		{`{"org.matrix.msc3874.not_rel_types": ["m.thread"]}`, []*rstypes.HeaderedEvent{root, reference}},
		{`{"org.matrix.msc3874.not_rel_types": ["m.reference"]}`, []*rstypes.HeaderedEvent{root, thread}},
		{`{"org.matrix.msc3874.not_rel_types": ["m.thread", "m.reference"]}`, []*rstypes.HeaderedEvent{root}},
		{`{"org.matrix.labels": ["#fun"]}`, []*rstypes.HeaderedEvent{root, reference}},
		{`{"org.matrix.labels": ["#fun"], "org.matrix.not_labels": ["#work"]}`, []*rstypes.HeaderedEvent{root}},
		{`{"org.matrix.not_labels": ["#fun"]}`, []*rstypes.HeaderedEvent{thread}},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			var timelineFilter map[string]any
			require.NoError(t, json.Unmarshal([]byte(tc.filter), &timelineFilter))
			// Only the messages sent above.
			timelineFilter["types"] = []string{"m.room.message"}
			roomEventFilter, err := json.Marshal(timelineFilter)
			require.NoError(t, err)

			body := get(fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages", room.ID), map[string]string{
				"dir": "f", "from": since, "filter": tc.filter,
			})
			assert.Equal(t, ids(tc.want...), chunk(body, "chunk"), "/messages")

			syncFilter, err := json.Marshal(map[string]any{"room": map[string]any{"timeline": timelineFilter}})
			require.NoError(t, err)
			res := r.sync(map[string]string{"since": since, "filter": string(syncFilter)})
			assert.ElementsMatch(t, ids(tc.want...), eventIDs(res, path, "timeline"), "incremental /sync")
			res = r.sync(map[string]string{"filter": string(syncFilter)})
			assert.Equal(t, ids(tc.want...), eventIDs(res, path, "timeline"), "initial /sync")

			body = get(fmt.Sprintf("/_matrix/client/v3/rooms/%s/context/%s", room.ID, root.EventID()), map[string]string{
				"filter": string(roomEventFilter),
			})
			var want []string
			for _, ev := range tc.want {
				if ev.EventID() != root.EventID() {
					want = append(want, ev.EventID())
				}
			}
			assert.Equal(t, want, chunk(body, "events_after"), "/context")
		})
	}

	t.Run("redaction removes labels", func(t *testing.T) {
		redaction := room.CreateAndInsert(t, alice, spec.MRoomRedaction, map[string]any{}, test.WithRedacts(reference.EventID()))
		r.send(redaction)
		r.syncUntil(alice, map[string]string{"since": end}, func(res string) bool {
			return len(eventIDs(res, path, "timeline")) > 0
		})
		require.Eventually(t, func() bool {
			body := get(fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages", room.ID), map[string]string{
				"dir": "f", "from": since, "filter": `{"org.matrix.labels": ["#work"]}`,
			})
			return len(chunk(body, "chunk")) == 0
		}, 5*time.Second, 50*time.Millisecond)
	})
}
