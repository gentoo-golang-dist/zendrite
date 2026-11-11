package clientapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	federationAPI "codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
)

// inviteRecordingFederationAPI stands in for the federation API and records
// the invite events the roomserver sends to remote servers. Any other method
// panics through the nil embedded interface.
type inviteRecordingFederationAPI struct {
	federationAPI.RoomserverFederationAPI
	mu      sync.Mutex
	invites []gomatrixserverlib.PDU
}

func (f *inviteRecordingFederationAPI) SendInvite(
	_ context.Context, event gomatrixserverlib.PDU, _ []gomatrixserverlib.InviteStrippedState,
) (gomatrixserverlib.PDU, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invites = append(f.invites, event)
	return event, nil
}

func (f *inviteRecordingFederationAPI) sent() []gomatrixserverlib.PDU {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gomatrixserverlib.PDU(nil), f.invites...)
}

// TestCreateRoomInviteReason covers MSC4491: the reason given to createRoom
// as uk.timedout.msc4491.invite_reason is the reason of every invite the
// request sends, to local and to remote users.
func TestCreateRoomInviteReason(t *testing.T) {
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	const remoteUser = "@carol:remote.example"

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		routers := httputil.NewRouters()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)

		fedAPI := &inviteRecordingFederationAPI{}
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(fedAPI, nil)
		userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
		AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, nil, caching.DisableMetrics)

		accessTokens := map[*test.User]userDevice{alice: {}}
		createAccessTokens(t, accessTokens, userAPI, processCtx.Context(), routers)

		createRoom := func(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
			t.Helper()
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v3/createRoom", strings.NewReader(string(raw)))
			req.Header.Set("Authorization", "Bearer "+accessTokens[alice].accessToken)
			routers.Client.ServeHTTP(w, req)
			return w
		}
		bobInvite := func(t *testing.T, w *httptest.ResponseRecorder) gomatrixserverlib.PDU {
			t.Helper()
			if w.Code != http.StatusOK {
				t.Fatalf("createRoom: HTTP %d: %s", w.Code, w.Body.String())
			}
			roomID, err := spec.NewRoomID(gjson.GetBytes(w.Body.Bytes(), "room_id").Str)
			if err != nil {
				t.Fatal(err)
			}
			ev, err := rsAPI.CurrentStateEvent(context.Background(), *roomID, spec.MRoomMember, bob.ID)
			if err != nil {
				t.Fatal(err)
			}
			if ev == nil {
				t.Fatal("no membership event for the invitee")
			}
			if got := gjson.GetBytes(ev.Content(), "membership").Str; got != spec.Invite {
				t.Fatalf("membership = %q, want invite", got)
			}
			return ev
		}

		t.Run("reason on local and remote invites", func(t *testing.T) {
			const reason = "Let's talk about the release"
			w := createRoom(t, map[string]any{
				"invite":                            []string{bob.ID, remoteUser},
				"is_direct":                         true,
				"uk.timedout.msc4491.invite_reason": reason,
			})
			ev := bobInvite(t, w)
			if got := gjson.GetBytes(ev.Content(), "reason").Str; got != reason {
				t.Fatalf("local invite reason = %q, want %q (content %s)", got, reason, ev.Content())
			}
			if !gjson.GetBytes(ev.Content(), "is_direct").Bool() {
				t.Fatalf("local invite lost is_direct: %s", ev.Content())
			}

			roomID := gjson.GetBytes(w.Body.Bytes(), "room_id").Str
			var remote gomatrixserverlib.PDU
			for _, sent := range fedAPI.sent() {
				if sent.RoomID().String() == roomID && sent.StateKey() != nil && *sent.StateKey() == remoteUser {
					remote = sent
				}
			}
			if remote == nil {
				t.Fatal("no invite was sent over federation to the remote invitee")
			}
			if got := gjson.GetBytes(remote.Content(), "reason").Str; got != reason {
				t.Fatalf("federated invite reason = %q, want %q (content %s)", got, reason, remote.Content())
			}
			if !gjson.GetBytes(remote.Content(), "is_direct").Bool() {
				t.Fatalf("federated invite lost is_direct: %s", remote.Content())
			}
		})

		// Like Synapse, a missing, empty or non-string reason, and the
		// stable field name while the MSC is unstable, leave the invite
		// without a reason rather than failing the request.
		for name, body := range map[string]map[string]any{
			"no reason":             {},
			"empty reason":          {"uk.timedout.msc4491.invite_reason": ""},
			"null reason":           {"uk.timedout.msc4491.invite_reason": nil},
			"number reason":         {"uk.timedout.msc4491.invite_reason": 42},
			"object reason":         {"uk.timedout.msc4491.invite_reason": map[string]any{"text": "hi"}},
			"stable field unstable": {"invite_reason": "not yet"},
		} {
			t.Run(name, func(t *testing.T) {
				body["invite"] = []string{bob.ID}
				ev := bobInvite(t, createRoom(t, body))
				if r := gjson.GetBytes(ev.Content(), "reason"); r.Exists() {
					t.Fatalf("invite has reason %s, want none (content %s)", r.Raw, ev.Content())
				}
			})
		}

		t.Run("too long reason", func(t *testing.T) {
			w := createRoom(t, map[string]any{
				"invite":                            []string{bob.ID},
				"uk.timedout.msc4491.invite_reason": strings.Repeat("a", 65537),
			})
			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("HTTP %d, want 413: %s", w.Code, w.Body.String())
			}
			if got := gjson.GetBytes(w.Body.Bytes(), "errcode").Str; got != string(spec.ErrorTooLarge) {
				t.Fatalf("errcode = %q, want M_TOO_LARGE", got)
			}
		})
	})
}
