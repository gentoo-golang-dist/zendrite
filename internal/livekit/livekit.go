// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

// Package livekit issues access tokens for LiveKit SFUs, as MSC4195 asks
// homeservers to do for the MatrixRTC LiveKit transport.
package livekit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// Tokens let their holder join for an hour: a self-hosted SFU cannot revoke
// tokens yet, so MSC4195 asks for a short lifetime. The token that creates
// rooms lasts a minute. At most 64 KiB of the SFU's answer is read. A created
// room is not created again for a minute; an empty LiveKit room is closed
// after a while, so it is not remembered for long. One requester may ask for
// 5 tokens a second, 20 at once.
const (
	tokenLifetime      = time.Hour
	adminTokenLifetime = time.Minute
	requestTimeout     = 10 * time.Second
	maxResponseBytes   = 1 << 16
	createdRoomTTL     = time.Minute
	requestsPerSecond  = 5
	requestBurst       = 20
)

// RoomName derives the LiveKit room name of a MatrixRTC slot, which does not
// reveal the Matrix room or slot to the SFU.
func RoomName(roomID, slotID string) string {
	return hashArray(roomID, slotID)
}

// ParticipantIdentity derives the LiveKit participant identity of a
// MatrixRTC member, from the sender of their m.rtc.member event and its
// member.id.
func ParticipantIdentity(userID, memberID string) string {
	return hashArray(userID, memberID)
}

// hashArray returns the unpadded base64 SHA-256 of the canonical JSON of an
// array of strings.
func hashArray(values ...string) string {
	// Canonical JSON of an array of strings is its compact encoding, with
	// non-ASCII characters left unescaped.
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(values); err != nil {
		// Encoding a slice of strings cannot fail.
		panic(err)
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// videoGrant is the LiveKit grant of a token.
type videoGrant struct {
	Room                 string `json:"room,omitempty"`
	RoomCreate           bool   `json:"roomCreate"`
	RoomJoin             bool   `json:"roomJoin,omitempty"`
	CanPublish           *bool  `json:"canPublish,omitempty"`
	CanSubscribe         *bool  `json:"canSubscribe,omitempty"`
	CanUpdateOwnMetadata *bool  `json:"canUpdateOwnMetadata,omitempty"`
}

type claims struct {
	Issuer    string     `json:"iss"`
	Subject   string     `json:"sub,omitempty"`
	NotBefore int64      `json:"nbf"`
	Expires   int64      `json:"exp"`
	Video     videoGrant `json:"video"`
}

// sign returns claims as a JWT signed with HS256, as LiveKit expects.
func sign(secret string, c claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Issuer issues access tokens for the SFUs whose API keys Zendrite holds.
type Issuer struct {
	foci   []config.RTCFocus
	client *http.Client
	now    func() time.Time

	mu sync.Mutex
	// The time each room was last created on an SFU, so that the members of
	// a call do not each ask the SFU.
	created map[string]time.Time
	// The rate limiters of the requesters, such as servers over federation.
	limiters map[string]*rate.Limiter
}

// NewIssuer returns an issuer for the SFUs of the foci that issue tokens.
func NewIssuer(foci []config.RTCFocus) *Issuer {
	return &Issuer{
		foci: foci, client: &http.Client{Timeout: requestTimeout}, now: time.Now,
		created: make(map[string]time.Time), limiters: make(map[string]*rate.Limiter),
	}
}

// Allow reports whether another token request of a requester, such as a
// server over federation, is within the rate limit.
func (i *Issuer) Allow(requester string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	limiter, ok := i.limiters[requester]
	if !ok {
		limiter = rate.NewLimiter(rate.Limit(requestsPerSecond), requestBurst)
		i.limiters[requester] = limiter
	}
	return limiter.Allow()
}

// Enabled reports whether Zendrite issues tokens for any SFU.
func (i *Issuer) Enabled() bool {
	for _, focus := range i.foci {
		if focus.IssuesTokens() {
			return true
		}
	}
	return false
}

// SFU returns the focus whose SFU is at url, if Zendrite issues its tokens.
func (i *Issuer) SFU(url string) *config.RTCFocus {
	for idx := range i.foci {
		focus := &i.foci[idx]
		if focus.IssuesTokens() && strings.TrimSuffix(focus.URL, "/") == strings.TrimSuffix(url, "/") {
			return focus
		}
	}
	return nil
}

// Token creates the LiveKit room of a MatrixRTC slot on the focus's SFU, if
// it does not exist yet, and returns a token that lets a member join it.
// Tokens never let their holder create or delete rooms, or hide.
func (i *Issuer) Token(ctx context.Context, focus *config.RTCFocus, roomID, slotID, userID, memberID string, canPublish bool) (string, error) {
	room := RoomName(roomID, slotID)
	if err := i.createRoom(ctx, focus, room); err != nil {
		return "", err
	}
	now := i.now()
	subscribe, noMetadata := true, false
	return sign(focus.LiveKitAPISecret, claims{
		Issuer:    focus.LiveKitAPIKey,
		Subject:   ParticipantIdentity(userID, memberID),
		NotBefore: now.Unix(),
		Expires:   now.Add(tokenLifetime).Unix(),
		Video: videoGrant{
			Room: room, RoomJoin: true,
			CanPublish: &canPublish, CanSubscribe: &subscribe, CanUpdateOwnMetadata: &noMetadata,
		},
	})
}

// createRoom creates a room on the SFU through LiveKit's RoomService API,
// which returns the existing room if there is one.
func (i *Issuer) createRoom(ctx context.Context, focus *config.RTCFocus, room string) error {
	now := i.now()
	key := focus.URL + "\x00" + room
	i.mu.Lock()
	if created, ok := i.created[key]; ok && now.Sub(created) < createdRoomTTL {
		i.mu.Unlock()
		return nil
	}
	for k, created := range i.created {
		if now.Sub(created) >= createdRoomTTL {
			delete(i.created, k)
		}
	}
	i.mu.Unlock()
	admin, err := sign(focus.LiveKitAPISecret, claims{
		Issuer:    focus.LiveKitAPIKey,
		NotBefore: now.Unix(),
		Expires:   now.Add(adminTokenLifetime).Unix(),
		Video:     videoGrant{RoomCreate: true},
	})
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"name": room})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL(focus)+"/twirp/livekit.RoomService/CreateRoom", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+admin)
	req.Header.Set("Content-Type", "application/json")
	resp, err := i.client.Do(req)
	if err != nil {
		return fmt.Errorf("creating the LiveKit room: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("creating the LiveKit room: the SFU answered %d", resp.StatusCode)
	}
	i.mu.Lock()
	i.created[key] = now
	i.mu.Unlock()
	return nil
}

// apiURL returns the SFU's HTTP API: livekit_api_url, or the WebSocket URL
// with an http(s) scheme.
func apiURL(focus *config.RTCFocus) string {
	if focus.LiveKitAPIURL != "" {
		return strings.TrimSuffix(focus.LiveKitAPIURL, "/")
	}
	url := strings.TrimSuffix(focus.URL, "/")
	switch {
	case strings.HasPrefix(url, "wss://"):
		return "https://" + strings.TrimPrefix(url, "wss://")
	case strings.HasPrefix(url, "ws://"):
		return "http://" + strings.TrimPrefix(url, "ws://")
	}
	return url
}
