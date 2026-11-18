// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/livekit"
	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// delegationUserAPI keeps delayed events in memory.
type delegationUserAPI struct {
	userapi.ClientUserAPI
	mu       sync.Mutex
	events   map[string]*userapi.DelayedEvent
	restarts map[string]int
}

func (u *delegationUserAPI) QueryDelayedEvent(_ context.Context, delayID string) (*userapi.DelayedEvent, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if event, ok := u.events[delayID]; ok {
		copied := *event
		return &copied, nil
	}
	return nil, nil
}

func (u *delegationUserAPI) PerformDelayedEventRestart(_ context.Context, delayID string, now spec.Timestamp) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	event, ok := u.events[delayID]
	if !ok || event.Status != userapi.DelayedEventScheduled {
		return false, nil
	}
	event.DelayedSince = now
	u.restarts[delayID]++
	return true, nil
}

func (u *delegationUserAPI) restartsOf(delayID string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.restarts[delayID]
}

func (u *delegationUserAPI) setStatus(delayID string, status userapi.DelayedEventStatus) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.events[delayID].Status = status
}

// delegationSFU answers GetParticipant and RemoveParticipant.
type delegationSFU struct {
	*httptest.Server
	mu        sync.Mutex
	connected map[string]bool
	removed   []string
}

func newDelegationSFU(t *testing.T) *delegationSFU {
	t.Helper()
	sfu := &delegationSFU{connected: map[string]bool{}}
	sfu.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Room, Identity string }
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		sfu.mu.Lock()
		defer sfu.mu.Unlock()
		switch strings.TrimPrefix(r.URL.Path, "/twirp/livekit.RoomService/") {
		case "GetParticipant":
			if !sfu.connected[body.Identity] {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"not_found"}`))
				return
			}
		case "RemoveParticipant":
			sfu.removed = append(sfu.removed, body.Identity)
			delete(sfu.connected, body.Identity)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(sfu.Close)
	return sfu
}

func (s *delegationSFU) set(identity string, connected bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected[identity] = connected
}

func (s *delegationSFU) removals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.removed...)
}

// delegationRoomserver answers membership queries.
type delegationRoomserver struct {
	roomserverAPI.ClientRoomserverAPI
	joined map[string]bool
}

func (r *delegationRoomserver) QueryMembershipForUser(_ context.Context, req *roomserverAPI.QueryMembershipForUserRequest, res *roomserverAPI.QueryMembershipForUserResponse) error {
	if r.joined[req.UserID.String()] {
		res.IsInRoom, res.Membership = true, spec.Join
	} else {
		res.Membership = spec.Leave
	}
	return nil
}

type delegationTest struct {
	l       *liveKitDelegations
	userAPI *delegationUserAPI
	sfu     *delegationSFU
	rsAPI   *delegationRoomserver
	issuer  *livekit.Issuer
	mu      sync.Mutex
	sent    []string
}

func newDelegationTest(t *testing.T) *delegationTest {
	t.Helper()
	sfu := newDelegationSFU(t)
	issuer := livekit.NewIssuer([]config.RTCFocus{{
		Type: "livekit", URL: "wss://sfu.example.com", LiveKitAPIKey: "key", LiveKitAPISecret: "secret", LiveKitAPIURL: sfu.URL,
	}})
	userAPI := &delegationUserAPI{events: map[string]*userapi.DelayedEvent{}, restarts: map[string]int{}}
	rsAPI := &delegationRoomserver{joined: map[string]bool{}}
	dt := &delegationTest{userAPI: userAPI, sfu: sfu, rsAPI: rsAPI, issuer: issuer}
	dt.l = newLiveKitDelegations(t.Context(), issuer, &DelayedEvents{userAPI: userAPI}, rsAPI)
	dt.l.pollInterval, dt.l.connectTimeout, dt.l.minDelay = 20*time.Millisecond, 300*time.Millisecond, 0
	dt.l.sendLeave = func(_ context.Context, d *delegation) {
		dt.mu.Lock()
		defer dt.mu.Unlock()
		dt.sent = append(dt.sent, d.delayID)
	}
	return dt
}

func (dt *delegationTest) sentLeaves() []string {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	return append([]string(nil), dt.sent...)
}

func (dt *delegationTest) schedule(delayID, userID, roomID string, delay time.Duration) {
	localpart, server, _ := strings.Cut(strings.TrimPrefix(userID, "@"), ":")
	dt.userAPI.mu.Lock()
	defer dt.userAPI.mu.Unlock()
	dt.userAPI.events[delayID] = &userapi.DelayedEvent{
		DelayID: delayID, Localpart: localpart, ServerName: spec.ServerName(server), RoomID: roomID,
		DelayMS: delay.Milliseconds(), DelayedSince: spec.AsTimestamp(time.Now()), Status: userapi.DelayedEventScheduled,
	}
}

func (dt *delegationTest) delegate(t *testing.T, body map[string]string) int {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v1/rtc/livekit/delegate_delayed_leave", bytes.NewReader(raw))
	return dt.l.Delegate(req, &userapi.Device{UserID: alice}).Code
}

func (dt *delegationTest) running() int {
	dt.l.mu.Lock()
	defer dt.l.mu.Unlock()
	return len(dt.l.byKey)
}

func delegateBody(delayID string) map[string]string {
	return map[string]string{"url": "wss://sfu.example.com/", "room_id": "!room:example.com", "slot_id": "slot", "member_id": "member", "delay_id": delayID}
}

const (
	alice = "@alice:example.com"
	bob   = "@bob:example.com"
)

var aliceIdentity = livekit.ParticipantIdentity(alice, "member")

func TestLiveKitDelegateRequest(t *testing.T) {
	dt := newDelegationTest(t)
	dt.schedule("alice", alice, "!room:example.com", time.Hour)
	dt.schedule("bob", bob, "!room:example.com", time.Hour)
	dt.schedule("elsewhere", alice, "!other:example.com", time.Hour)
	dt.schedule("sent", alice, "!room:example.com", time.Hour)
	dt.userAPI.setStatus("sent", userapi.DelayedEventFinalized)

	missing := delegateBody("alice")
	delete(missing, "member_id")
	require.Equal(t, http.StatusBadRequest, dt.delegate(t, missing))
	otherSFU := delegateBody("alice")
	otherSFU["url"] = "wss://other.example.com"
	require.Equal(t, http.StatusBadRequest, dt.delegate(t, otherSFU))
	for _, delayID := range []string{"unknown", "bob", "elsewhere", "sent"} {
		require.Equal(t, http.StatusBadRequest, dt.delegate(t, delegateBody(delayID)), delayID)
	}
	require.Zero(t, dt.running())

	// Delays shorter than ten seconds are refused.
	dt.l.minDelay = liveKitMinDelegatedDelay
	dt.schedule("short", alice, "!room:example.com", 9*time.Second)
	require.Equal(t, http.StatusBadRequest, dt.delegate(t, delegateBody("short")))
	require.Zero(t, dt.running())

	require.Equal(t, http.StatusOK, dt.delegate(t, delegateBody("alice")))
	require.Equal(t, 1, dt.running())

	// Delegating another delayed event for the same member replaces the
	// first delegation.
	dt.schedule("alice 2", alice, "!room:example.com", time.Hour)
	require.Equal(t, http.StatusOK, dt.delegate(t, delegateBody("alice 2")))
	require.Equal(t, 1, dt.running())
	dt.l.mu.Lock()
	require.Nil(t, dt.l.byDelayID["alice"])
	require.NotNil(t, dt.l.byDelayID["alice 2"])
	dt.l.mu.Unlock()
}

func TestLiveKitDelegation(t *testing.T) {
	t.Run("sent when the participant disconnects", func(t *testing.T) {
		dt := newDelegationTest(t)
		// Restarted every 100ms.
		dt.schedule("leave", alice, "!room:example.com", 200*time.Millisecond)
		dt.sfu.set(aliceIdentity, true)
		require.Equal(t, http.StatusOK, dt.delegate(t, delegateBody("leave")))
		require.Eventually(t, func() bool { return dt.userAPI.restartsOf("leave") >= 3 }, 5*time.Second, 10*time.Millisecond)
		require.Empty(t, dt.sentLeaves(), "not sent while connected, beyond its delay")

		dt.sfu.set(aliceIdentity, false)
		require.Eventually(t, func() bool { return len(dt.sentLeaves()) == 1 }, 5*time.Second, 10*time.Millisecond)
		require.Eventually(t, func() bool { return dt.running() == 0 }, 5*time.Second, 10*time.Millisecond)
		require.Equal(t, []string{"leave"}, dt.sentLeaves())
	})

	t.Run("sent when the participant never connects", func(t *testing.T) {
		dt := newDelegationTest(t)
		dt.schedule("leave", alice, "!room:example.com", time.Hour)
		require.Equal(t, http.StatusOK, dt.delegate(t, delegateBody("leave")))
		time.Sleep(100 * time.Millisecond)
		require.Empty(t, dt.sentLeaves(), "the participant may still connect")
		require.Eventually(t, func() bool { return len(dt.sentLeaves()) == 1 }, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("webhooks", func(t *testing.T) {
		dt := newDelegationTest(t)
		dt.l.pollInterval = time.Hour
		dt.schedule("leave", alice, "!room:example.com", time.Hour)
		require.Equal(t, http.StatusOK, dt.delegate(t, delegateBody("leave")))
		participant := livekit.Participant{
			Focus: dt.issuer.SFU("wss://sfu.example.com"), Room: livekit.RoomName("!room:example.com", "slot"), Identity: aliceIdentity,
		}
		dt.sfu.set(aliceIdentity, true)
		dt.l.signal(participant, delegationSignal{connected: true})
		// A late disconnect of a participant who is connected again, and a
		// connection replaced by a newer one, send nothing.
		dt.l.signal(participant, delegationSignal{})
		dt.l.signal(participant, delegationSignal{replaced: true})
		time.Sleep(100 * time.Millisecond)
		require.Empty(t, dt.sentLeaves())
		require.Equal(t, 1, dt.running())

		dt.sfu.set(aliceIdentity, false)
		dt.l.signal(participant, delegationSignal{})
		require.Eventually(t, func() bool { return len(dt.sentLeaves()) == 1 }, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("a disconnect the SFU cannot confirm is not trusted", func(t *testing.T) {
		dt := newDelegationTest(t)
		dt.l.pollInterval = time.Hour
		dt.schedule("leave", alice, "!room:example.com", time.Hour)
		require.Equal(t, http.StatusOK, dt.delegate(t, delegateBody("leave")))
		participant := livekit.Participant{
			Focus: dt.issuer.SFU("wss://sfu.example.com"), Room: livekit.RoomName("!room:example.com", "slot"), Identity: aliceIdentity,
		}
		dt.l.signal(participant, delegationSignal{connected: true})
		dt.sfu.Close()
		dt.l.signal(participant, delegationSignal{})
		time.Sleep(100 * time.Millisecond)
		require.Empty(t, dt.sentLeaves())
		require.Equal(t, 1, dt.running())
	})

	t.Run("stops when the delayed event is no longer scheduled", func(t *testing.T) {
		dt := newDelegationTest(t)
		dt.schedule("leave", alice, "!room:example.com", 200*time.Millisecond)
		dt.sfu.set(aliceIdentity, true)
		require.Equal(t, http.StatusOK, dt.delegate(t, delegateBody("leave")))
		dt.userAPI.setStatus("leave", userapi.DelayedEventFinalized)
		require.Eventually(t, func() bool { return dt.running() == 0 }, 5*time.Second, 10*time.Millisecond)
		dt.sfu.set(aliceIdentity, false)
		time.Sleep(100 * time.Millisecond)
		require.Empty(t, dt.sentLeaves())
	})

	t.Run("left to its delay while the SFU does not answer", func(t *testing.T) {
		dt := newDelegationTest(t)
		dt.schedule("leave", alice, "!room:example.com", time.Hour)
		dt.sfu.Close()
		require.Equal(t, http.StatusOK, dt.delegate(t, delegateBody("leave")))
		require.Eventually(t, func() bool { return dt.running() == 0 }, 5*time.Second, 10*time.Millisecond)
		require.Empty(t, dt.sentLeaves())
	})
}

func TestLiveKitWebhook(t *testing.T) {
	dt := newDelegationTest(t)
	focus := dt.issuer.SFU("wss://sfu.example.com")
	room := livekit.RoomName("!room:example.com", "slot")
	webhookBody := func(body []byte, secret string) int {
		sum := sha256.Sum256(body)
		payload, err := json.Marshal(map[string]any{"iss": "key", "exp": time.Now().Add(time.Minute).Unix(), "sha256": base64.StdEncoding.EncodeToString(sum[:])})
		require.NoError(t, err)
		unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(unsigned))
		req := httptest.NewRequest(http.MethodPost, "/_zendrite/livekit/webhook", bytes.NewReader(body))
		req.Header.Set("Authorization", unsigned+"."+base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
		return dt.l.Webhook(req).Code
	}
	webhook := func(event, identity, secret string) int {
		body, err := json.Marshal(map[string]any{"event": event, "room": map[string]string{"name": room}, "participant": map[string]string{"identity": identity}})
		require.NoError(t, err)
		return webhookBody(body, secret)
	}
	require.Equal(t, http.StatusUnauthorized, webhook(livekit.EventParticipantJoined, aliceIdentity, "wrong"))
	require.Equal(t, http.StatusBadRequest, webhookBody([]byte(`{"event":1}`), "secret"))

	// Tokens were issued to Alice and Bob, who is still joined.
	for _, user := range []string{alice, bob} {
		_, err := dt.issuer.Token(t.Context(), focus, "!room:example.com", "slot", user, "member", true)
		require.NoError(t, err)
	}
	dt.rsAPI.joined[bob] = true
	bobIdentity := livekit.ParticipantIdentity(bob, "member")
	require.Equal(t, http.StatusOK, webhook(livekit.EventParticipantJoined, bobIdentity, "secret"))
	require.Equal(t, http.StatusOK, webhook(livekit.EventParticipantJoined, "unknown", "secret"))
	require.Empty(t, dt.sfu.removals(), "joined members and participants of other token issuers stay")

	// Alice left the room but rejoins the SFU with her token.
	require.Equal(t, http.StatusOK, webhook(livekit.EventParticipantJoined, aliceIdentity, "secret"))
	require.Equal(t, []string{aliceIdentity}, dt.sfu.removals())

	// Events that are not about participants joining or leaving are ignored.
	require.Equal(t, http.StatusOK, webhook("room_started", "", "secret"))
}
