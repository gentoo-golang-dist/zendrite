package clientapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
)

// TestIdenticalStateFromAnotherSender checks that sending state identical to
// the room's current state only short-circuits for the user who sent it.
// Anyone else's request is checked against the auth rules, so a room creator
// cannot set state under another user's ID just because that user already set
// the same content (Complement msc3757/TestWithoutOwnedState).
func TestIdenticalStateFromAnotherSender(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	room := test.NewRoom(t, alice)
	room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(bob.ID))
	room.CreateAndInsert(t, alice, spec.MRoomPowerLevels, map[string]any{
		"users": map[string]any{alice.ID: 100, bob.ID: 50},
	}, test.WithStateKey(""))
	existing := room.CreateAndInsert(t, bob, "com.example.test", map[string]any{}, test.WithStateKey(bob.ID))
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
		cfg.ClientAPI.RateLimiting.Enabled = false
		defer closeRig()
		natsInstance := jetstream.NATSInstance{}
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		rsAPI.SetUserAPI(userAPI)
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, nil, caching.DisableMetrics) //nolint:contextcheck

		accessTokens := map[*test.User]userDevice{alice: {}, bob: {}}
		createAccessTokens(t, accessTokens, userAPI, ctx, routers)

		if err := api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false); err != nil {
			t.Fatal(err)
		}

		put := func(user *test.User) *httptest.ResponseRecorder {
			path := "/_matrix/client/v3/rooms/" + url.PathEscape(room.ID) + "/state/com.example.test/" + url.PathEscape(bob.ID)
			req := httptest.NewRequest(http.MethodPut, path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+accessTokens[user].accessToken)
			w := httptest.NewRecorder()
			routers.Client.ServeHTTP(w, req)
			return w
		}

		// The room creator may not set state under Bob's user ID, even with
		// the content Bob already set.
		if w := put(alice); w.Code != http.StatusForbidden {
			t.Fatalf("alice: got HTTP %d, want 403: %s", w.Code, w.Body.String())
		}

		// Bob resending his own state is idempotent.
		w := put(bob)
		if w.Code != http.StatusOK {
			t.Fatalf("bob: got HTTP %d, want 200: %s", w.Code, w.Body.String())
		}
		if got := gjson.GetBytes(w.Body.Bytes(), "event_id").Str; got != existing.EventID() {
			t.Fatalf("bob: got event %s, want the existing %s", got, existing.EventID())
		}
	})
}
