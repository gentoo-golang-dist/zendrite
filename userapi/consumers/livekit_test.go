// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/livekit"
	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
)

// TestRemoveFromLiveKit checks that members who leave or are banned from a
// room are removed from the LiveKit rooms they were issued tokens for
// (MSC4195), and that nothing else removes anyone.
func TestRemoveFromLiveKit(t *testing.T) {
	var mu sync.Mutex
	var removed []string
	sfu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/twirp/livekit.RoomService/RemoveParticipant" {
			var body struct{ Identity string }
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			removed = append(removed, body.Identity)
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer sfu.Close()
	removals := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), removed...)
	}

	global := &config.Global{RTCFoci: []config.RTCFocus{{
		Type: "livekit", URL: "wss://sfu.test", LiveKitAPIKey: "key", LiveKitAPISecret: "secret", LiveKitAPIURL: sfu.URL,
	}}}
	issuer := livekit.For(global)
	alice := test.NewUser(t)
	bob := test.NewUser(t)
	carol := test.NewUser(t)
	room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
	for _, user := range []*test.User{bob, carol} {
		room.CreateAndInsert(t, user, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(user.ID))
		_, err := issuer.Token(t.Context(), issuer.SFU("wss://sfu.test"), room.ID, "slot", user.ID, "member", true)
		require.NoError(t, err)
	}
	message := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "hi"})
	bobLeave := room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(bob.ID))
	carolBan := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Ban}, test.WithStateKey(carol.ID))

	s := &OutputRoomEventConsumer{cfg: &config.UserAPI{Matrix: global}}
	for _, output := range []*rsapi.OutputNewRoomEvent{
		{Event: message, AddsStateEventIDs: []string{message.EventID()}},
		// A leave that does not change the state removes no one.
		{Event: bobLeave},
	} {
		s.removeFromLiveKit(t.Context(), output)
	}
	time.Sleep(50 * time.Millisecond)
	require.Empty(t, removals())

	s.removeFromLiveKit(t.Context(), &rsapi.OutputNewRoomEvent{Event: bobLeave, AddsStateEventIDs: []string{bobLeave.EventID()}})
	require.Eventually(t, func() bool { return len(removals()) == 1 }, 5*time.Second, 10*time.Millisecond)
	s.removeFromLiveKit(t.Context(), &rsapi.OutputNewRoomEvent{Event: carolBan, AddsStateEventIDs: []string{carolBan.EventID()}})
	require.Eventually(t, func() bool { return len(removals()) == 2 }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, []string{livekit.ParticipantIdentity(bob.ID, "member"), livekit.ParticipantIdentity(carol.ID, "member")}, removals())
}
