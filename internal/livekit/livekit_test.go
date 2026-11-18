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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// The test vectors of the MSC4195 appendix.
func TestDerivations(t *testing.T) {
	require.Equal(t, "O8437W3+jmzMVjoIP3tNwbm+XxHQk2iKpOA7aqw3qSc", RoomName("!roomid:example.com", "slot1234"))
	require.Equal(t, "M3VnsLXrkbxIDINXO64u8PZzFyD9ZYFiQULR2dshWYs", ParticipantIdentity("@alice:example.com", "memberABC"))
}

// decode checks a JWT's HS256 signature and returns its claims.
func decode(t *testing.T, token, secret string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	require.Equal(t, base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), parts[2], "signature")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var c map[string]any
	require.NoError(t, json.Unmarshal(payload, &c))
	return c
}

func TestToken(t *testing.T) {
	var created []map[string]string
	sfu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/twirp/livekit.RoomService/CreateRoom", r.URL.Path)
		admin := decode(t, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "secret")
		video, ok := admin["video"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, true, video["roomCreate"])
		var body map[string]string
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &body))
		created = append(created, body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer sfu.Close()

	focus := config.RTCFocus{
		Type: "livekit", URL: "wss://sfu.example.com/", LiveKitAPIKey: "key", LiveKitAPISecret: "secret", LiveKitAPIURL: sfu.URL,
	}
	issuer := NewIssuer([]config.RTCFocus{{Type: "livekit", LiveKitServiceURL: "https://jwt.example.com"}, focus})
	now := time.Unix(1_700_000_000, 0)
	issuer.now = func() time.Time { return now }
	require.True(t, issuer.Enabled())
	require.Nil(t, issuer.SFU("wss://other.example.com"))
	sfuFocus := issuer.SFU("wss://sfu.example.com")
	require.NotNil(t, sfuFocus, "a trailing slash does not matter")

	token, err := issuer.Token(t.Context(), sfuFocus, "!room:example.com", "slot", "@alice:example.com", "member", true)
	require.NoError(t, err)
	c := decode(t, token, "secret")
	require.Equal(t, "key", c["iss"])
	require.Equal(t, ParticipantIdentity("@alice:example.com", "member"), c["sub"])
	require.EqualValues(t, now.Unix(), c["nbf"])
	require.EqualValues(t, now.Add(time.Hour).Unix(), c["exp"])
	require.Equal(t, map[string]any{
		"room": RoomName("!room:example.com", "slot"), "roomCreate": false, "roomJoin": true,
		"canPublish": true, "canSubscribe": true, "canUpdateOwnMetadata": false,
	}, c["video"])
	require.Equal(t, []map[string]string{{"name": RoomName("!room:example.com", "slot")}}, created)

	// The room is not created again for the next member.
	_, err = issuer.Token(t.Context(), sfuFocus, "!room:example.com", "slot", "@bob:example.com", "member", true)
	require.NoError(t, err)
	require.Len(t, created, 1)

	// A failing SFU gives no token for a room it has to create.
	sfu.Close()
	_, err = issuer.Token(t.Context(), sfuFocus, "!room:example.com", "other slot", "@alice:example.com", "member", true)
	require.Error(t, err)
}

func TestAllow(t *testing.T) {
	issuer := NewIssuer(nil)
	allowed := 0
	for range requestBurst + 5 {
		if issuer.Allow("remote.example") {
			allowed++
		}
	}
	require.Equal(t, requestBurst, allowed)
	require.True(t, issuer.Allow("other.example"), "each requester has its own limit")
}

func TestAPIURL(t *testing.T) {
	require.Equal(t, "https://sfu.example.com/livekit", apiURL(&config.RTCFocus{URL: "wss://sfu.example.com/livekit/"}))
	require.Equal(t, "http://localhost:7880", apiURL(&config.RTCFocus{URL: "ws://localhost:7880"}))
	require.Equal(t, "https://api.example.com", apiURL(&config.RTCFocus{URL: "wss://sfu.example.com", LiveKitAPIURL: "https://api.example.com/"}))
}
