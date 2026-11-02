// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package syncapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"golang.org/x/crypto/bcrypt"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
	userstorage "codefloe.com/pat-s/zendrite/userapi/storage"
)

// threadSubscriptionsDB serves the userapi's thread subscriptions API from
// its storage, as the userapi does.
type threadSubscriptionsDB struct {
	db userstorage.UserDatabase
}

func newThreadSubscriptionsDB(ctx context.Context, cm *sqlutil.Connections, cfg *config.Zendrite) userapi.ThreadSubscriptionsAPI {
	db, err := userstorage.NewUserDatabase(ctx, cm, &cfg.UserAPI.AccountDatabase, cfg.Global.ServerName, bcrypt.MinCost, config.DefaultOpenIDTokenLifetimeMS, time.Minute, "")
	if err != nil {
		panic(err)
	}
	return threadSubscriptionsDB{db: db}
}

func splitUserID(userID string) (string, spec.ServerName) {
	localpart, domain, err := gomatrixserverlib.SplitID('@', userID)
	if err != nil {
		panic(err)
	}
	return localpart, domain
}

func (d threadSubscriptionsDB) PerformThreadSubscribe(ctx context.Context, req *userapi.PerformThreadSubscribeRequest) (*userapi.PerformThreadSubscribeResponse, error) {
	localpart, domain := splitUserID(req.UserID)
	id, conflict, err := d.db.SubscribeThread(ctx, localpart, domain, req.RoomID, req.ThreadRootID, req.Automatic, req.CauseStreamPos)
	return &userapi.PerformThreadSubscribeResponse{StreamID: id, Conflict: conflict}, err
}

func (d threadSubscriptionsDB) PerformThreadUnsubscribe(ctx context.Context, req *userapi.PerformThreadUnsubscribeRequest) (int64, error) {
	localpart, domain := splitUserID(req.UserID)
	return d.db.UnsubscribeThread(ctx, localpart, domain, req.RoomID, req.ThreadRootID, req.RoomStreamPos)
}

func (d threadSubscriptionsDB) QueryThreadSubscription(ctx context.Context, userID, roomID, threadRootID string) (*userapi.ThreadSubscription, error) {
	localpart, domain := splitUserID(userID)
	return d.db.GetThreadSubscription(ctx, localpart, domain, roomID, threadRootID)
}

func (d threadSubscriptionsDB) QueryThreadSubscriptionUpdates(ctx context.Context, userID string, from, to int64, limit int) ([]userapi.ThreadSubscriptionUpdate, error) {
	localpart, domain := splitUserID(userID)
	return d.db.GetThreadSubscriptionUpdates(ctx, localpart, domain, from, to, limit)
}

func (d threadSubscriptionsDB) QueryMaxThreadSubscriptionStreamID(ctx context.Context) (int64, error) {
	return d.db.MaxThreadSubscriptionStreamID(ctx)
}

// threadSubscription calls the MSC4306 endpoint.
func (e *stickyEnv) threadSubscription(user *test.User, method, roomID, rootID string, body any) (int, gjson.Result) {
	e.t.Helper()
	opts := []test.HTTPRequestOpt{test.WithQueryParams(map[string]string{"access_token": e.devices[user.ID].AccessToken})}
	if body != nil {
		opts = append(opts, test.WithJSONBody(e.t, body))
	}
	w := httptest.NewRecorder()
	path := "/_matrix/client/unstable/io.element.msc4306/rooms/" + url.PathEscape(roomID) + "/thread/" + url.PathEscape(rootID) + "/subscription"
	e.routers.Client.ServeHTTP(w, test.NewRequest(e.t, method, path, opts...))
	return w.Code, gjson.Parse(w.Body.String())
}

// threadSubscriptionsPage calls the MSC4308 endpoint.
func (e *stickyEnv) threadSubscriptionsPage(user *test.User, params map[string]string) (int, gjson.Result) {
	e.t.Helper()
	query := map[string]string{"access_token": e.devices[user.ID].AccessToken}
	for k, v := range params {
		query[k] = v
	}
	w := httptest.NewRecorder()
	e.routers.Client.ServeHTTP(w, test.NewRequest(e.t, "GET", "/_matrix/client/unstable/io.element.msc4308/thread_subscriptions", test.WithQueryParams(query)))
	return w.Code, gjson.Parse(w.Body.String())
}

// threadSubsSync makes a sliding sync request with the MSC4308 extension.
func (e *stickyEnv) threadSubsSync(user *test.User, pos string, ext map[string]any) gjson.Result {
	e.t.Helper()
	body, err := json.Marshal(map[string]any{
		"lists":      dummyLists,
		"extensions": map[string]any{"io.element.msc4308.thread_subscriptions": ext},
	})
	require.NoError(e.t, err)
	params := map[string]string{"access_token": e.devices[user.ID].AccessToken, "timeout": "0"}
	if pos != "" {
		params["pos"] = pos
	}
	w := httptest.NewRecorder()
	e.routers.Client.ServeHTTP(w, test.NewRequest(e.t, "POST", "/_matrix/client/unstable/org.matrix.simplified_msc3575/sync",
		test.WithQueryParams(params), test.WithJSONBody(e.t, json.RawMessage(body))))
	require.Equal(e.t, 200, w.Code, w.Body.String())
	return gjson.Parse(w.Body.String())
}

func threadSubsExtension(res gjson.Result) gjson.Result {
	return res.Get(`extensions.io\.element\.msc4308\.thread_subscriptions`)
}

func threadReply(t *testing.T, room *test.Room, user *test.User, root *rstypes.HeaderedEvent) *rstypes.HeaderedEvent {
	return room.CreateAndInsert(t, user, "m.room.message", map[string]any{
		"body":         "reply",
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root.EventID()},
	})
}

// The MSC4306 endpoints follow Synapse: manual subscriptions win over
// automatic ones, automatic ones need an event in the thread, and an
// automatic subscription caused by an event the user had seen when they
// unsubscribed is refused.
func TestThreadSubscriptionEndpoints(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice, bob, carol := test.NewUser(t), test.NewUser(t), test.NewUser(t)
		env := newSyncTestEnv(t, dbType, newThreadSubscriptionsDB, alice, bob, carol)
		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": "join"}, test.WithStateKey(bob.ID))
		root := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "root"})
		reply := threadReply(t, room, bob, root)
		plain := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "not in a thread"})
		env.send(room.Events()...)
		env.waitFor(alice, room.ID, plain)
		rootID := root.EventID()

		// Bad identifiers and unknown or invisible threads.
		code, res := env.threadSubscription(alice, "GET", room.ID, "root", nil)
		require.Equal(t, http.StatusBadRequest, code, res.Raw)
		require.Equal(t, "M_INVALID_PARAM", res.Get("errcode").Str)
		code, _ = env.threadSubscription(alice, "GET", "room", rootID, nil)
		require.Equal(t, http.StatusBadRequest, code)
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			code, res = env.threadSubscription(alice, method, room.ID, "$unknown", map[string]any{})
			require.Equal(t, http.StatusNotFound, code, "%s: %s", method, res.Raw)
			require.Equal(t, "M_NOT_FOUND", res.Get("errcode").Str)
			// Carol never joined the room, whose history is shared with members only.
			code, _ = env.threadSubscription(carol, method, room.ID, rootID, map[string]any{})
			require.Equal(t, http.StatusNotFound, code, method)
		}
		// A root in another room is not found either.
		code, _ = env.threadSubscription(alice, "GET", "!other:test", rootID, nil)
		require.Equal(t, http.StatusNotFound, code)

		// No subscription yet.
		code, res = env.threadSubscription(alice, "GET", room.ID, rootID, nil)
		require.Equal(t, http.StatusNotFound, code, res.Raw)

		// Bodies: automatic must be an event ID.
		code, res = env.threadSubscription(alice, "PUT", room.ID, rootID, map[string]any{"automatic": true})
		require.Equal(t, http.StatusBadRequest, code, res.Raw)
		require.Equal(t, "M_BAD_JSON", res.Get("errcode").Str)
		code, res = env.threadSubscription(alice, "PUT", room.ID, rootID, json.RawMessage(`[]`))
		require.Equal(t, http.StatusBadRequest, code, res.Raw)

		// An automatic subscription needs an event in the thread.
		code, res = env.threadSubscription(alice, "PUT", room.ID, rootID, map[string]any{"automatic": plain.EventID()})
		require.Equal(t, http.StatusBadRequest, code, res.Raw)
		require.Equal(t, "IO.ELEMENT.MSC4306.M_NOT_IN_THREAD", res.Get("errcode").Str)
		code, res = env.threadSubscription(alice, "PUT", room.ID, rootID, map[string]any{"automatic": rootID})
		require.Equal(t, http.StatusBadRequest, code, res.Raw)
		require.Equal(t, "IO.ELEMENT.MSC4306.M_NOT_IN_THREAD", res.Get("errcode").Str)
		code, res = env.threadSubscription(alice, "PUT", room.ID, rootID, map[string]any{"automatic": "$unknown"})
		require.Equal(t, http.StatusNotFound, code, res.Raw)

		// Automatic, then manual, which an automatic one cannot downgrade.
		code, res = env.threadSubscription(alice, "PUT", room.ID, rootID, map[string]any{"automatic": reply.EventID()})
		require.Equal(t, http.StatusOK, code, res.Raw)
		require.JSONEq(t, `{}`, res.Raw)
		code, res = env.threadSubscription(alice, "GET", room.ID, rootID, nil)
		require.Equal(t, http.StatusOK, code, res.Raw)
		require.JSONEq(t, `{"automatic":true}`, res.Raw)
		code, _ = env.threadSubscription(alice, "PUT", room.ID, rootID, map[string]any{})
		require.Equal(t, http.StatusOK, code)
		code, _ = env.threadSubscription(alice, "PUT", room.ID, rootID, map[string]any{"automatic": reply.EventID()})
		require.Equal(t, http.StatusOK, code)
		_, res = env.threadSubscription(alice, "GET", room.ID, rootID, nil)
		require.JSONEq(t, `{"automatic":false}`, res.Raw)

		// Unsubscribing is idempotent.
		for range 2 {
			code, res = env.threadSubscription(alice, "DELETE", room.ID, rootID, nil)
			require.Equal(t, http.StatusOK, code, res.Raw)
			require.JSONEq(t, `{}`, res.Raw)
		}
		code, _ = env.threadSubscription(alice, "GET", room.ID, rootID, nil)
		require.Equal(t, http.StatusNotFound, code)

		// The reply arrived before the unsubscription: a client that had not
		// seen the unsubscription cannot undo it with that reply.
		code, res = env.threadSubscription(alice, "PUT", room.ID, rootID, map[string]any{"automatic": reply.EventID()})
		require.Equal(t, http.StatusConflict, code, res.Raw)
		require.Equal(t, "IO.ELEMENT.MSC4306.M_CONFLICTING_UNSUBSCRIPTION", res.Get("errcode").Str)
		code, _ = env.threadSubscription(alice, "GET", room.ID, rootID, nil)
		require.Equal(t, http.StatusNotFound, code)

		// A later reply does cause an automatic subscription.
		later := threadReply(t, room, bob, root)
		env.send(later)
		env.waitFor(alice, room.ID, later)
		code, res = env.threadSubscription(alice, "PUT", room.ID, rootID, map[string]any{"automatic": later.EventID()})
		require.Equal(t, http.StatusOK, code, res.Raw)
		_, res = env.threadSubscription(alice, "GET", room.ID, rootID, nil)
		require.JSONEq(t, `{"automatic":true}`, res.Raw)

		// Bob's subscriptions are his own.
		code, _ = env.threadSubscription(bob, "GET", room.ID, rootID, nil)
		require.Equal(t, http.StatusNotFound, code)
	})
}

// The MSC4308 sliding sync extension returns the latest change per thread
// since the connection position, with prev_batch when it left older changes
// out, and the companion endpoint pages back through them.
func TestThreadSubscriptionsSlidingSync(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice, bob := test.NewUser(t), test.NewUser(t)
		env := newSyncTestEnv(t, dbType, newThreadSubscriptionsDB, alice, bob)
		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		root1 := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "root 1"})
		root2 := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "root 2"})
		root3 := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "root 3"})
		env.send(room.Events()...)
		env.waitFor(alice, room.ID, root3)
		enabled := map[string]any{"enabled": true}
		roomKey := gjson.Escape(room.ID)

		res := env.threadSubsSync(alice, "", enabled)
		require.False(t, threadSubsExtension(res).Exists(), "nothing yet: %s", res.Raw)
		pos := res.Get("pos").Str

		code, _ := env.threadSubscription(alice, "PUT", room.ID, root1.EventID(), map[string]any{})
		require.Equal(t, http.StatusOK, code)
		res = env.threadSubsSync(alice, pos, enabled)
		ext := threadSubsExtension(res)
		sub := ext.Get("subscribed." + roomKey + "." + gjson.Escape(root1.EventID()))
		require.True(t, sub.Exists(), res.Raw)
		require.False(t, sub.Get("automatic").Bool())
		bump1 := sub.Get("bump_stamp").Int()
		require.Positive(t, bump1)
		require.False(t, ext.Get("unsubscribed").Exists())
		require.False(t, ext.Get("prev_batch").Exists())
		pos = res.Get("pos").Str

		// Nothing changed since.
		res = env.threadSubsSync(alice, pos, enabled)
		require.False(t, threadSubsExtension(res).Exists(), res.Raw)
		pos = res.Get("pos").Str

		// Several changes: only the latest state of each thread is sent.
		for _, root := range []*rstypes.HeaderedEvent{root2, root3} {
			code, _ = env.threadSubscription(alice, "PUT", room.ID, root.EventID(), map[string]any{})
			require.Equal(t, http.StatusOK, code)
		}
		code, _ = env.threadSubscription(alice, "DELETE", room.ID, root1.EventID(), nil)
		require.Equal(t, http.StatusOK, code)
		res = env.threadSubsSync(alice, pos, enabled)
		ext = threadSubsExtension(res)
		require.Len(t, ext.Get("subscribed."+roomKey).Map(), 2, res.Raw)
		unsub := ext.Get("unsubscribed." + roomKey + "." + gjson.Escape(root1.EventID()))
		require.Greater(t, unsub.Get("bump_stamp").Int(), bump1, res.Raw)
		require.False(t, unsub.Get("automatic").Exists())

		// Bob sees none of Alice's changes.
		res = env.threadSubsSync(bob, "", enabled)
		require.False(t, threadSubsExtension(res).Exists(), res.Raw)

		// With a limit, the latest changes win, and prev_batch pages back
		// through the endpoint down to the request's pos.
		res = env.threadSubsSync(alice, pos, map[string]any{"enabled": true, "limit": 1})
		ext = threadSubsExtension(res)
		require.True(t, ext.Get("unsubscribed."+roomKey+"."+gjson.Escape(root1.EventID())).Exists(), res.Raw)
		require.False(t, ext.Get("subscribed").Exists(), res.Raw)
		prevBatch := ext.Get("prev_batch").Str
		require.NotEmpty(t, prevBatch, res.Raw)
		// The connection only keeps its latest position.
		latestPos := res.Get("pos").Str

		code, page := env.threadSubscriptionsPage(alice, map[string]string{"dir": "b", "from": prevBatch, "to": pos, "limit": "1"})
		require.Equal(t, http.StatusOK, code, page.Raw)
		require.True(t, page.Get("subscribed."+roomKey+"."+gjson.Escape(root3.EventID())).Exists(), page.Raw)
		require.NotEmpty(t, page.Get("end").Str, page.Raw)
		code, page = env.threadSubscriptionsPage(alice, map[string]string{"dir": "b", "from": page.Get("end").Str, "to": pos, "limit": "1"})
		require.Equal(t, http.StatusOK, code, page.Raw)
		require.True(t, page.Get("subscribed."+roomKey+"."+gjson.Escape(root2.EventID())).Exists(), page.Raw)
		code, page = env.threadSubscriptionsPage(alice, map[string]string{"dir": "b", "from": page.Get("end").Str, "to": pos, "limit": "1"})
		require.Equal(t, http.StatusOK, code, page.Raw)
		require.JSONEq(t, `{}`, page.Raw)

		// Without bounds the endpoint covers everything, latest state first.
		code, page = env.threadSubscriptionsPage(alice, map[string]string{"dir": "b"})
		require.Equal(t, http.StatusOK, code, page.Raw)
		require.Len(t, page.Get("subscribed."+roomKey).Map(), 2, page.Raw)
		require.Len(t, page.Get("unsubscribed."+roomKey).Map(), 1, page.Raw)
		require.False(t, page.Get("end").Exists(), page.Raw)

		for _, params := range []map[string]string{
			{},
			{"dir": "f"},
			{"dir": "b", "limit": "0"},
			{"dir": "b", "limit": "x"},
			{"dir": "b", "from": "s1_2"},
			{"dir": "b", "to": "nonsense"},
		} {
			code, page = env.threadSubscriptionsPage(alice, params)
			require.Equal(t, http.StatusBadRequest, code, "%v: %s", params, page.Raw)
		}

		// A change wakes a waiting sliding sync request.
		done := make(chan gjson.Result, 1)
		go func() {
			body, _ := json.Marshal(map[string]any{
				"lists":      dummyLists,
				"extensions": map[string]any{"io.element.msc4308.thread_subscriptions": enabled},
			})
			w := httptest.NewRecorder()
			env.routers.Client.ServeHTTP(w, test.NewRequest(t, "POST", "/_matrix/client/unstable/org.matrix.simplified_msc3575/sync",
				test.WithQueryParams(map[string]string{"access_token": env.devices[alice.ID].AccessToken, "timeout": "10000", "pos": latestPos}),
				test.WithJSONBody(t, json.RawMessage(body))))
			done <- gjson.Parse(w.Body.String())
		}()
		time.Sleep(200 * time.Millisecond)
		code, _ = env.threadSubscription(alice, "PUT", room.ID, root1.EventID(), map[string]any{})
		require.Equal(t, http.StatusOK, code)
		select {
		case res = <-done:
			require.True(t, threadSubsExtension(res).Get("subscribed."+roomKey+"."+gjson.Escape(root1.EventID())).Exists(), res.Raw)
		case <-time.After(8 * time.Second):
			t.Fatal("the sliding sync request was not woken by the subscription")
		}
	})
}
