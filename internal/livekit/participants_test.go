// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package livekit

import (
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

	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// fakeSFU serves LiveKit's RoomService API for the participants it holds.
type fakeSFU struct {
	*httptest.Server
	mu        sync.Mutex
	connected map[string]bool
	removed   []map[string]any
	failing   bool
}

func newFakeSFU(t *testing.T) *fakeSFU {
	t.Helper()
	sfu := &fakeSFU{connected: map[string]bool{}}
	sfu.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sfu.mu.Lock()
		defer sfu.mu.Unlock()
		if sfu.failing {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		claims := decode(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "secret")
		video, _ := claims["video"].(map[string]any)
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &body))
		method := strings.TrimPrefix(r.URL.Path, "/twirp/livekit.RoomService/")
		if method != "CreateRoom" {
			require.Equal(t, true, video["roomAdmin"], method)
			require.Equal(t, body["room"], video["room"], method)
		}
		key, _ := body["room"].(string)
		identity, _ := body["identity"].(string)
		key += "/" + identity
		switch method {
		case "CreateRoom":
		case "GetParticipant":
			if !sfu.connected[key] {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"not_found","msg":"participant does not exist"}`))
				return
			}
		case "RemoveParticipant":
			sfu.removed = append(sfu.removed, body)
			if !sfu.connected[key] {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":"not_found","msg":"participant does not exist"}`))
				return
			}
			delete(sfu.connected, key)
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(sfu.Close)
	return sfu
}

func (s *fakeSFU) connect(p Participant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected[p.Room+"/"+p.Identity] = true
}

func (s *fakeSFU) removals() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.removed...)
}

func newTestIssuer(sfu *fakeSFU) (*Issuer, *config.RTCFocus) {
	issuer := NewIssuer([]config.RTCFocus{{
		Type: "livekit", URL: "wss://sfu.example.com", LiveKitAPIKey: "key", LiveKitAPISecret: "secret", LiveKitAPIURL: sfu.URL,
	}})
	return issuer, issuer.SFU("wss://sfu.example.com")
}

func TestParticipants(t *testing.T) {
	sfu := newFakeSFU(t)
	issuer, focus := newTestIssuer(sfu)
	now := time.Unix(1_700_000_000, 0)
	issuer.now = func() time.Time { return now }

	_, err := issuer.Token(t.Context(), focus, "!room:example.com", "slot", "@alice:example.com", "member", true)
	require.NoError(t, err)
	alice := Participant{Focus: focus, Room: RoomName("!room:example.com", "slot"), Identity: ParticipantIdentity("@alice:example.com", "member")}

	roomID, userID, ok := issuer.Member(focus, alice.Room, alice.Identity)
	require.True(t, ok)
	require.Equal(t, "!room:example.com", roomID)
	require.Equal(t, "@alice:example.com", userID)
	_, _, ok = issuer.Member(focus, alice.Room, "someone else")
	require.False(t, ok)
	require.Equal(t, []Participant{alice}, issuer.participants.of("!room:example.com", "@alice:example.com", now))
	require.Empty(t, issuer.participants.of("!other:example.com", "@alice:example.com", now))

	connected, err := issuer.Connected(t.Context(), alice)
	require.NoError(t, err)
	require.False(t, connected)
	sfu.connect(alice)
	connected, err = issuer.Connected(t.Context(), alice)
	require.NoError(t, err)
	require.True(t, connected)

	// Leaving the room removes the participant and revokes their tokens.
	issuer.MembershipEnded(t.Context(), "!room:example.com", "@alice:example.com")
	require.Eventually(t, func() bool { return len(sfu.removals()) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, map[string]any{"room": alice.Room, "identity": alice.Identity, "revoke_token_ts": float64(now.Unix())}, sfu.removals()[0])
	connected, err = issuer.Connected(t.Context(), alice)
	require.NoError(t, err)
	require.False(t, connected)

	// A participant who is not connected is no error.
	require.NoError(t, issuer.RemoveParticipant(t.Context(), alice))
	// Nor is anyone removed for a member without tokens.
	issuer.MembershipEnded(t.Context(), "!room:example.com", "@bob:example.com")

	// A failing SFU is.
	sfu.mu.Lock()
	sfu.failing = true
	sfu.mu.Unlock()
	require.Error(t, issuer.RemoveParticipant(t.Context(), alice))
	_, err = issuer.Connected(t.Context(), alice)
	require.Error(t, err)

	// Participants are forgotten when their tokens expire.
	now = now.Add(tokenLifetime)
	_, _, ok = issuer.Member(focus, alice.Room, alice.Identity)
	require.False(t, ok)
	require.Empty(t, issuer.participants.of("!room:example.com", "@alice:example.com", now))
	sfu.mu.Lock()
	sfu.failing = false
	sfu.mu.Unlock()
	_, err = issuer.Token(t.Context(), focus, "!room:example.com", "slot", "@bob:example.com", "member", true)
	require.NoError(t, err)
	require.Len(t, issuer.participants.issued, 1, "the expired participant was pruned")
}

func TestFor(t *testing.T) {
	global := &config.Global{}
	require.Same(t, For(global), For(global))
	require.NotSame(t, For(global), For(&config.Global{}))
}

// webhookToken signs a webhook body as LiveKit does.
func webhookToken(t *testing.T, key, secret string, body []byte, exp time.Time) string {
	t.Helper()
	sum := sha256.Sum256(body)
	payload, err := json.Marshal(map[string]any{
		"iss": key, "nbf": exp.Add(-5 * time.Minute).Unix(), "exp": exp.Unix(),
		"sha256": base64.StdEncoding.EncodeToString(sum[:]),
	})
	require.NoError(t, err)
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestWebhook(t *testing.T) {
	issuer := NewIssuer([]config.RTCFocus{
		{Type: "livekit", LiveKitServiceURL: "https://jwt.example.com"},
		{Type: "livekit", URL: "wss://a.example.com", LiveKitAPIKey: "a", LiveKitAPISecret: "secret a"},
		{Type: "livekit", URL: "wss://b.example.com", LiveKitAPIKey: "b", LiveKitAPISecret: "secret b"},
		// Another region of b's SFU, with the same key.
		{Type: "livekit", URL: "wss://b2.example.com", LiveKitAPIKey: "b", LiveKitAPISecret: "secret b"},
	})
	now := time.Unix(1_700_000_000, 0)
	issuer.now = func() time.Time { return now }
	body := []byte(`{"event":"participant_left","room":{"name":"room","sid":"RM_1"},"participant":{"identity":"id","disconnectReason":"DUPLICATE_IDENTITY"},"id":"EV_1","createdAt":"1700000000"}`)
	exp := now.Add(5 * time.Minute)

	event, err := issuer.Webhook(webhookToken(t, "b", "secret b", body, exp), body)
	require.NoError(t, err)
	require.Equal(t, EventParticipantLeft, event.Event)
	require.Equal(t, []Participant{
		{Focus: issuer.SFU("wss://b.example.com"), Room: "room", Identity: "id"},
		{Focus: issuer.SFU("wss://b2.example.com"), Room: "room", Identity: "id"},
	}, event.Subjects())
	require.True(t, event.Replaced())
	event, err = issuer.Webhook("Bearer "+webhookToken(t, "a", "secret a", body, exp), body)
	require.NoError(t, err, "a bearer token")
	require.Len(t, event.Subjects(), 1)

	for name, auth := range map[string]string{
		"no token":           "",
		"wrong secret":       webhookToken(t, "b", "secret a", body, exp),
		"unknown key":        webhookToken(t, "c", "secret b", body, exp),
		"other body":         webhookToken(t, "b", "secret b", []byte(`{}`), exp),
		"expired":            webhookToken(t, "b", "secret b", body, now.Add(-2*time.Minute)),
		"not a token":        "a.b.c",
		"too many parts":     webhookToken(t, "b", "secret b", body, exp) + ".x",
		"lk-jwt-service key": webhookToken(t, "", "", body, exp),
		"no expiry":          webhookToken(t, "b", "secret b", body, time.Unix(0, 0)),
	} {
		_, err = issuer.Webhook(auth, body)
		require.Error(t, err, name)
	}

	for reason, replaced := range map[string]bool{`2`: true, `"CLIENT_INITIATED"`: false, `1`: false, `null`: false} {
		event = &WebhookEvent{}
		event.Participant.DisconnectReason = json.RawMessage(reason)
		require.Equal(t, replaced, event.Replaced(), reason)
	}
}
