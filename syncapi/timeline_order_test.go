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

// TestSyncTimelineOrder checks MSC4445: a sync that sends a room for the
// first time (an initial sync, a newly joined room, a room new to a sliding
// sync connection) returns its latest events in topological order, and an
// incremental sync returns what arrived since, in stream order.
//
// The room forks after its creation. The main branch arrives first and
// reaches deeper into the DAG; the other branch arrives after it, as when a
// server rejoins after a netsplit, so its events come last in the stream
// though they are older in the DAG.
func TestSyncTimelineOrder(t *testing.T) {
	test.WithAllDatabases(t, testSyncTimelineOrder)
}

//nolint:gocyclo
func testSyncTimelineOrder(t *testing.T, dbType test.DBType) {
	alice, bob := test.NewUser(t), test.NewUser(t)
	aliceDev := userapi.Device{ID: "ALICEID", UserID: alice.ID, AccessToken: "ALICE_BEARER_TOKEN", AccountType: userapi.AccountTypeUser}
	bobDev := userapi.Device{ID: "BOBID", UserID: bob.ID, AccessToken: "BOB_BEARER_TOKEN", AccountType: userapi.AccountTypeUser}

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
	AddPublicRoutes(processCtx, routers, cfg, cm, &natsInstance, accountDataUserAPI{&syncUserAPI{accounts: []userapi.Device{aliceDev, bobDev}}}, rsAPI, caches, caching.DisableMetrics, nil, nil)

	r := &stateAfterRig{
		t: t, routers: routers, rsAPI: rsAPI, alice: alice,
		tokens: map[*test.User]string{alice: aliceDev.AccessToken, bob: bobDev.AccessToken},
	}

	room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
	path := "rooms.join." + gjson.Escape(room.ID)
	created := slices.Clone(room.Events())
	forkPoint := created[len(created)-1]
	message := func(body string) *rstypes.HeaderedEvent {
		return room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": body})
	}
	// The late branch: depths just after the fork point.
	late := make([]*rstypes.HeaderedEvent, 20)
	for i := range late {
		late[i] = message(fmt.Sprintf("late %d", i+1))
	}
	// The main branch, also from the fork point, deeper than the late one.
	mainBranch := make([]*rstypes.HeaderedEvent, 30)
	mainBranch[0] = room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "main 1"}, test.WithPrevEvents(forkPoint.EventID()))
	for i := 1; i < len(mainBranch); i++ {
		mainBranch[i] = message(fmt.Sprintf("main %d", i+1))
	}
	require.Less(t, late[len(late)-1].Depth(), mainBranch[0].Depth())

	ids := func(events ...*rstypes.HeaderedEvent) []string {
		out := make([]string, len(events))
		for i, ev := range events {
			out[i] = ev.EventID()
		}
		return out
	}
	filter := func(limit int) string {
		b, err := json.Marshal(map[string]any{"room": map[string]any{"timeline": map[string]any{"limit": limit}}})
		require.NoError(t, err)
		return string(b)
	}
	inTimeline := func(eventID string) func(string) bool {
		return func(res string) bool {
			return slices.Contains(eventIDs(res, path, "timeline"), eventID)
		}
	}

	// The main branch arrives first.
	r.send(append(slices.Clone(created), mainBranch...)...)
	res := r.syncUntil(alice, map[string]string{"filter": filter(5)}, inTimeline(mainBranch[len(mainBranch)-1].EventID()))
	since := gjson.Get(res, "next_batch").Str

	// Then the late branch: the stream delivers it after the mainBranch one.
	r.send(late...)

	t.Run("incremental sync is in stream order", func(t *testing.T) {
		res := r.syncUntil(alice, map[string]string{"filter": filter(5), "since": since}, inTimeline(late[len(late)-1].EventID()))
		assert.Equal(t, ids(late[15:]...), eventIDs(res, path, "timeline"))
		assert.True(t, gjson.Get(res, path+".timeline.limited").Bool())
	})

	var prevBatch string
	t.Run("initial sync is in topological order", func(t *testing.T) {
		res := r.sync(map[string]string{"filter": filter(5)})
		assert.Equal(t, ids(mainBranch[25:]...), eventIDs(res, path, "timeline"),
			"the latest events of the DAG, not those that arrived last")
		assert.True(t, gjson.Get(res, path+".timeline.limited").Bool())
		prevBatch = gjson.Get(res, path+".timeline.prev_batch").Str
		require.NotEmpty(t, prevBatch)
	})

	t.Run("prev_batch continues before the timeline", func(t *testing.T) {
		w := httptest.NewRecorder()
		routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages", room.ID), test.WithQueryParams(map[string]string{
			"access_token": aliceDev.AccessToken, "dir": "b", "from": prevBatch, "limit": "5",
		})))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var got []string
		for _, ev := range gjson.GetBytes(w.Body.Bytes(), "chunk").Array() {
			got = append(got, ev.Get("event_id").Str)
		}
		assert.Equal(t, ids(test.Reversed(mainBranch[20:25])...), got)
	})

	t.Run("sliding sync sends a new room in topological order", func(t *testing.T) {
		body := map[string]any{"lists": map[string]any{"all": map[string]any{"ranges": [][]int{{0, 10}}, "timeline_limit": 5}}}
		w := httptest.NewRecorder()
		routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodPost, "/_matrix/client/unstable/org.matrix.simplified_msc3575/sync",
			test.WithQueryParams(map[string]string{"access_token": aliceDev.AccessToken}), test.WithJSONBody(t, body)))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		roomData := gjson.GetBytes(w.Body.Bytes(), "rooms."+gjson.Escape(room.ID))
		var got []string
		for _, ev := range roomData.Get("timeline").Array() {
			got = append(got, ev.Get("event_id").Str)
		}
		assert.Equal(t, ids(mainBranch[25:]...), got)
		assert.True(t, roomData.Get("limited").Bool())
	})

	t.Run("a newly joined room is in topological order", func(t *testing.T) {
		res := r.syncAs(bob, http.StatusOK, map[string]string{"filter": filter(5)})
		bobSince := gjson.Get(res, "next_batch").Str
		join := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		r.send(join)
		res = r.syncUntil(bob, map[string]string{"filter": filter(5), "since": bobSince}, inTimeline(join.EventID()))
		assert.Equal(t, ids(append(slices.Clone(mainBranch[26:]), join)...), eventIDs(res, path, "timeline"))
		assert.True(t, gjson.Get(res, path+".timeline.limited").Bool())
	})
}
