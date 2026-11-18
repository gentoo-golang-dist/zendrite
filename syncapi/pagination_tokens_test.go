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

// TestPaginationTokensAreDirectionless checks MSC4492: a token from
// /messages, /relations, /context or /sync names a position in the room, and
// paginating from it in either direction, on any of /messages and /relations,
// continues from that position.
func TestPaginationTokensAreDirectionless(t *testing.T) {
	test.WithAllDatabases(t, testPaginationTokensAreDirectionless)
}

//nolint:gocyclo
func testPaginationTokensAreDirectionless(t *testing.T, dbType test.DBType) {
	alice := test.NewUser(t)
	aliceDev := userapi.Device{ID: "ALICEID", UserID: alice.ID, AccessToken: "ALICE_BEARER_TOKEN", AccountType: userapi.AccountTypeUser}
	bob := test.NewUser(t)
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

	r := &stateAfterRig{t: t, routers: routers, rsAPI: rsAPI, alice: alice, tokens: map[*test.User]string{alice: aliceDev.AccessToken, bob: bobDev.AccessToken}}

	// A root message followed by eight messages; every other one is a thread
	// reply to the root, so /relations sees the replies and /messages sees all.
	room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
	room.CreateAndInsert(t, bob, "m.room.member", map[string]any{"membership": "join"}, test.WithStateKey(bob.ID))
	root := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "root"})
	var timeline, replies []*rstypes.HeaderedEvent
	for i := range 8 {
		content := map[string]any{"msgtype": "m.text", "body": fmt.Sprintf("message %d", i)}
		if i%2 == 0 {
			content["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": root.EventID()}
		}
		ev := room.CreateAndInsert(t, alice, "m.room.message", content)
		timeline = append(timeline, ev)
		if i%2 == 0 {
			replies = append(replies, ev)
		}
	}
	r.send(room.Events()...)
	last := timeline[len(timeline)-1].EventID()
	// A limited timeline, so that prev_batch points into the room.
	syncRes := r.syncUntil(alice, map[string]string{"filter": `{"room":{"timeline":{"limit":3}}}`}, func(res string) bool {
		return slices.Contains(eventIDs(res, "rooms.join."+gjson.Escape(room.ID), "timeline"), last)
	})
	prevBatch := gjson.Get(syncRes, "rooms.join."+gjson.Escape(room.ID)+".timeline.prev_batch").Str
	nextBatch := gjson.Get(syncRes, "next_batch").Str
	require.NotEmpty(t, prevBatch)
	require.NotEmpty(t, nextBatch)
	syncTimelineIDs := eventIDs(syncRes, "rooms.join."+gjson.Escape(room.ID), "timeline")

	// Name events by their place in the timeline, so failures are readable.
	names := map[string]string{root.EventID(): "root"}
	for i, ev := range timeline {
		names[ev.EventID()] = fmt.Sprintf("t%d", i)
	}
	name := func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		return id
	}
	ids := func(events ...*rstypes.HeaderedEvent) []string {
		out := make([]string, len(events))
		for i, ev := range events {
			out[i] = name(ev.EventID())
		}
		return out
	}
	getAs := func(token, path string, params map[string]string) gjson.Result {
		t.Helper()
		query := map[string]string{"access_token": token}
		for k, v := range params {
			if v != "" {
				query[k] = v
			}
		}
		w := httptest.NewRecorder()
		routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodGet, path, test.WithQueryParams(query)))
		require.Equal(t, http.StatusOK, w.Code, "%s %v: %s", path, params, w.Body.String())
		return gjson.ParseBytes(w.Body.Bytes())
	}
	get := func(path string, params map[string]string) gjson.Result {
		t.Helper()
		return getAs(aliceDev.AccessToken, path, params)
	}
	chunk := func(res gjson.Result) []string {
		out := []string{}
		for _, ev := range res.Get("chunk").Array() {
			out = append(out, name(ev.Get("event_id").Str))
		}
		return out
	}
	messagesPath := fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages", room.ID)
	relationsPath := fmt.Sprintf("/_matrix/client/v1/rooms/%s/relations/%s", room.ID, root.EventID())
	messages := func(dir, from, limit string) gjson.Result {
		t.Helper()
		return get(messagesPath, map[string]string{"dir": dir, "from": from, "limit": limit})
	}
	relations := func(dir, from, limit string) gjson.Result {
		t.Helper()
		return get(relationsPath, map[string]string{"dir": dir, "from": from, "limit": limit})
	}
	// Each check pages both ways from a token that names the position between
	// timeline[split-1] and timeline[split].
	checkMessages := func(t *testing.T, token string, split int) {
		t.Helper()
		before := ids(test.Reversed(timeline[max(split-2, 0):split])...)
		after := ids(timeline[split:min(split+2, len(timeline))]...)
		assert.Equal(t, before, chunk(messages("b", token, fmt.Sprint(len(before)))), "/messages dir=b")
		assert.Equal(t, after, chunk(messages("f", token, fmt.Sprint(len(after)))), "/messages dir=f")
	}
	checkRelations := func(t *testing.T, token string, split int) {
		t.Helper()
		var before, after []*rstypes.HeaderedEvent
		for i, ev := range timeline {
			if i%2 != 0 {
				continue
			}
			if i < split {
				before = append(before, ev)
			} else {
				after = append(after, ev)
			}
		}
		assert.Equal(t, ids(test.Reversed(before)...), chunk(relations("b", token, "")), "/relations dir=b")
		assert.Equal(t, ids(after...), chunk(relations("f", token, "")), "/relations dir=f")
	}
	check := func(t *testing.T, token string, split int) {
		t.Helper()
		require.NotEmpty(t, token)
		checkMessages(t, token, split)
		checkRelations(t, token, split)
	}

	t.Run("messages end token going backwards", func(t *testing.T) {
		res := messages("b", "", "3")
		require.Equal(t, ids(test.Reversed(timeline[5:])...), chunk(res))
		check(t, res.Get("end").Str, 5)
	})
	t.Run("messages end token going forwards", func(t *testing.T) {
		res := messages("f", messages("b", "", "3").Get("end").Str, "2")
		require.Equal(t, ids(timeline[5:7]...), chunk(res))
		check(t, res.Get("end").Str, 7)
	})
	t.Run("messages start token", func(t *testing.T) {
		end := messages("b", "", "3").Get("end").Str
		res := messages("b", end, "2")
		require.Equal(t, ids(timeline[4], timeline[3]), chunk(res))
		check(t, res.Get("start").Str, 5)
	})
	t.Run("relations next_batch going backwards", func(t *testing.T) {
		res := relations("b", "", "2")
		require.Equal(t, ids(replies[3], replies[2]), chunk(res))
		check(t, res.Get("next_batch").Str, 4)
	})
	t.Run("relations next_batch going forwards", func(t *testing.T) {
		res := relations("f", "", "2")
		require.Equal(t, ids(replies[0], replies[1]), chunk(res))
		check(t, res.Get("next_batch").Str, 3)
	})
	t.Run("context tokens", func(t *testing.T) {
		res := get(fmt.Sprintf("/_matrix/client/v3/rooms/%s/context/%s", room.ID, timeline[4].EventID()), map[string]string{"limit": "4"})
		require.Equal(t, timeline[4].EventID(), res.Get("event.event_id").Str)
		require.Equal(t, ids(timeline[3], timeline[2]), idsOf(res.Get("events_before"), name))
		require.Equal(t, ids(timeline[5], timeline[6]), idsOf(res.Get("events_after"), name))
		check(t, res.Get("start").Str, 2)
		check(t, res.Get("end").Str, 7)
	})
	t.Run("context tokens without context events", func(t *testing.T) {
		res := get(fmt.Sprintf("/_matrix/client/v3/rooms/%s/context/%s", room.ID, timeline[4].EventID()), map[string]string{"limit": "0"})
		require.Empty(t, idsOf(res.Get("events_before"), name))
		require.Empty(t, idsOf(res.Get("events_after"), name))
		check(t, res.Get("start").Str, 4)
		check(t, res.Get("end").Str, 5)
	})
	t.Run("sync prev_batch", func(t *testing.T) {
		split := len(timeline) - len(syncTimelineIDs)
		syncTimeline := []string{}
		for _, id := range syncTimelineIDs {
			syncTimeline = append(syncTimeline, name(id))
		}
		require.Equal(t, ids(timeline[split:]...), syncTimeline)
		check(t, prevBatch, split)
	})
	t.Run("sync next_batch", func(t *testing.T) {
		check(t, nextBatch, len(timeline))
	})
	t.Run("a backward page that reaches the create event has no end", func(t *testing.T) {
		res := messages("b", "", "100")
		require.Equal(t, "m.room.create", res.Get("chunk.@reverse.0.type").Str)
		assert.False(t, res.Get("end").Exists(), res.Get("end").Str)
	})
	t.Run("a user who left pages on from an earlier page", func(t *testing.T) {
		leave := room.CreateAndInsert(t, bob, "m.room.member", map[string]any{"membership": "leave"}, test.WithStateKey(bob.ID))
		after := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "after the leave"})
		r.send(leave, after)
		r.syncUntil(alice, nil, func(res string) bool {
			return slices.Contains(eventIDs(res, "rooms.join."+gjson.Escape(room.ID), "timeline"), after.EventID())
		})
		bobMessages := func(from string) gjson.Result {
			t.Helper()
			return getAs(bobDev.AccessToken, messagesPath, map[string]string{"dir": "b", "from": from, "limit": "3"})
		}
		first := bobMessages("")
		assert.Equal(t, []string{leave.EventID(), timeline[7].EventID(), timeline[6].EventID()}, idsOf(first.Get("chunk"), func(id string) string { return id }))
		second := bobMessages(first.Get("end").Str)
		assert.Equal(t, ids(timeline[5], timeline[4], timeline[3]), chunk(second))
	})
}

func idsOf(events gjson.Result, name func(string) string) []string {
	out := []string{}
	for _, ev := range events.Array() {
		out = append(out, name(ev.Get("event_id").Str))
	}
	return out
}
