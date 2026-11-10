package syncapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// TestSlidingSyncListRoomUpdates checks that a list sends the rooms in its
// range that changed, also when the range itself did not change, and leaves
// out the rooms that did not. Lists carry only their count, as in MSC4186.
func TestSlidingSyncListRoomUpdates(t *testing.T) {
	test.WithAllDatabases(t, testSlidingSyncListRoomUpdates)
}

func testSlidingSyncListRoomUpdates(t *testing.T, dbType test.DBType) {
	alice := test.NewUser(t)
	aliceDev := userapi.Device{ID: "ALICEID", UserID: alice.ID, AccessToken: "ALICE_BEARER_TOKEN", AccountType: userapi.AccountTypeUser}

	cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
	defer closeRig()
	routers := httputil.NewRouters()
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	natsInstance := jetstream.NATSInstance{}
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	AddPublicRoutes(processCtx, routers, cfg, cm, &natsInstance, accountDataUserAPI{&syncUserAPI{accounts: []userapi.Device{aliceDev}}}, rsAPI, caches, caching.DisableMetrics, nil, nil)
	jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
	defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)

	slidingSync := func(t *testing.T, pos string) gjson.Result {
		t.Helper()
		body := map[string]any{"lists": map[string]any{"all": map[string]any{"ranges": [][]int{{0, 10}}, "timeline_limit": 5}}}
		params := map[string]string{"access_token": aliceDev.AccessToken, "timeout": "0"}
		if pos != "" {
			params["pos"] = pos
		}
		w := httptest.NewRecorder()
		routers.Client.ServeHTTP(w, test.NewRequest(t, http.MethodPost, "/_matrix/client/unstable/org.matrix.simplified_msc3575/sync",
			test.WithQueryParams(params), test.WithJSONBody(t, body)))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		return gjson.ParseBytes(w.Body.Bytes())
	}
	send := func(t *testing.T, room *test.Room, body string) string {
		t.Helper()
		ev := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": body})
		require.NoError(t, rsapi.SendEvents(context.Background(), rsAPI, rsapi.KindNew, []*rstypes.HeaderedEvent{ev}, "test", "test", "test", nil, false))
		syncUntil(t, routers, aliceDev.AccessToken, false, func(syncBody string) bool {
			path := fmt.Sprintf(`rooms.join.%s.timeline.events.#(event_id=="%s")`, gjson.Escape(room.ID), ev.EventID())
			return gjson.Get(syncBody, path).Exists()
		})
		return ev.EventID()
	}

	room := test.NewRoom(t, alice)
	require.NoError(t, rsapi.SendEvents(context.Background(), rsAPI, rsapi.KindNew, room.Events(), "test", "test", "test", nil, false))
	first := send(t, room, "first")

	res := slidingSync(t, "")
	require.JSONEq(t, `{"all":{"count":1}}`, res.Get("lists").Raw, res.Raw)
	require.True(t, res.Get(fmt.Sprintf(`rooms.%s.timeline.#(event_id=="%s")`, gjson.Escape(room.ID), first)).Exists(), res.Raw)
	pos := res.Get("pos").Str

	// Nothing changed: the room is left out.
	res = slidingSync(t, pos)
	require.False(t, res.Get("rooms."+gjson.Escape(room.ID)).Exists(), res.Raw)
	pos = res.Get("pos").Str

	// A new message keeps the room where it is in the list, and is still sent.
	second := send(t, room, "second")
	res = slidingSync(t, pos)
	require.JSONEq(t, `{"all":{"count":1}}`, res.Get("lists").Raw, res.Raw)
	timeline := res.Get("rooms." + gjson.Escape(room.ID) + ".timeline")
	require.True(t, timeline.Get(fmt.Sprintf(`#(event_id=="%s")`, second)).Exists(), res.Raw)
	require.False(t, timeline.Get(fmt.Sprintf(`#(event_id=="%s")`, first)).Exists(), res.Raw)
}
