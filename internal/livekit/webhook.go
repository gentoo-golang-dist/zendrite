// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package livekit

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// webhookLeeway is how far the clocks of Zendrite and an SFU may disagree
// about a webhook token's validity.
const webhookLeeway = time.Minute

// The webhook events that MSC4195 needs.
const (
	EventParticipantJoined            = "participant_joined"
	EventParticipantLeft              = "participant_left"
	EventParticipantConnectionAborted = "participant_connection_aborted"
)

// WebhookEvent is the part of a LiveKit webhook event that Zendrite reads.
type WebhookEvent struct {
	// Foci are the foci whose API key signed the event: the SFU that sent
	// it, and any other that shares its key.
	Foci  []*config.RTCFocus `json:"-"`
	Event string             `json:"event"`
	Room  struct {
		Name string `json:"name"`
	} `json:"room"`
	Participant struct {
		Identity string `json:"identity"`
		// DisconnectReason is an enum, which LiveKit sends by name, or by
		// number in older versions.
		DisconnectReason json.RawMessage `json:"disconnectReason"`
	} `json:"participant"`
}

// Subjects returns the participant the event is about, on each SFU that may
// have sent it.
func (e *WebhookEvent) Subjects() []Participant {
	subjects := make([]Participant, 0, len(e.Foci))
	for _, focus := range e.Foci {
		subjects = append(subjects, Participant{Focus: focus, Room: e.Room.Name, Identity: e.Participant.Identity})
	}
	return subjects
}

// Replaced reports whether a participant left because another connection of
// theirs took their place, so that they are still connected: LiveKit's
// DUPLICATE_IDENTITY disconnect reason, whose number is 2.
func (e *WebhookEvent) Replaced() bool {
	reason := strings.Trim(string(e.Participant.DisconnectReason), `"`)
	return reason == "DUPLICATE_IDENTITY" || reason == "2"
}

var errWebhookAuth = errors.New("the webhook is not signed by an SFU of this server")

// ErrWebhookBody is the error of a verified webhook whose body is not an
// event.
var ErrWebhookBody = errors.New("the webhook body is not a LiveKit event")

// Webhook verifies a webhook request of one of Zendrite's SFUs, whose
// Authorization header carries a token signed with the SFU's API secret that
// holds the SHA-256 of the body, and decodes its event.
func (i *Issuer) Webhook(authorization string, body []byte) (*WebhookEvent, error) {
	token := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(authorization), "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) != 3 { //nolint:mnd // header, payload and signature
		return nil, errWebhookAuth
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errWebhookAuth
	}
	var c struct {
		Issuer    string `json:"iss"`
		NotBefore int64  `json:"nbf"`
		Expires   int64  `json:"exp"`
		SHA256    string `json:"sha256"`
	}
	if err = json.Unmarshal(payload, &c); err != nil || c.Issuer == "" {
		return nil, errWebhookAuth
	}
	var header struct {
		Algorithm string `json:"alg"`
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(rawHeader, &header) != nil || header.Algorithm != "HS256" {
		return nil, errWebhookAuth
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errWebhookAuth
	}
	var foci []*config.RTCFocus
	for idx := range i.foci {
		candidate := &i.foci[idx]
		if !candidate.IssuesTokens() || candidate.LiveKitAPIKey != c.Issuer {
			continue
		}
		mac := hmac.New(sha256.New, []byte(candidate.LiveKitAPISecret))
		mac.Write([]byte(parts[0] + "." + parts[1]))
		if hmac.Equal(mac.Sum(nil), signature) {
			foci = append(foci, candidate)
		}
	}
	if len(foci) == 0 {
		return nil, errWebhookAuth
	}
	now := i.now()
	if c.Expires == 0 || now.After(time.Unix(c.Expires, 0).Add(webhookLeeway)) ||
		(c.NotBefore != 0 && now.Before(time.Unix(c.NotBefore, 0).Add(-webhookLeeway))) {
		return nil, errWebhookAuth
	}
	sum := sha256.Sum256(body)
	if subtle.ConstantTimeCompare([]byte(c.SHA256), []byte(base64.StdEncoding.EncodeToString(sum[:]))) != 1 {
		return nil, errWebhookAuth
	}
	var event WebhookEvent
	if err = json.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWebhookBody, err)
	}
	event.Foci = foci
	return &event, nil
}
