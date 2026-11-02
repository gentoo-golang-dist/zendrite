// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
)

// policySignClient answers /sign requests per path with a fixed result.
type policySignClient struct {
	fclient.FederationClient
	answers map[string]func(result any) error
	paths   []string
}

func (c *policySignClient) DoRequestAndParseResponse(_ context.Context, req *http.Request, result any) error {
	c.paths = append(c.paths, req.URL.Path)
	if req.Method != http.MethodPost || req.Header.Get("Authorization") == "" {
		return errors.New("unsigned or wrong method")
	}
	answer, ok := c.answers[req.URL.Path]
	if !ok {
		return spec.HTTPError{Code: http.StatusNotFound, Contents: []byte(`{"errcode":"M_UNRECOGNIZED"}`)}
	}
	return answer(result)
}

func signed(result any) error {
	return json.Unmarshal([]byte(`{"policy.example":{"ed25519:policy_server":"c2lnbmF0dXJl"}}`), result)
}

func httpError(code int, body string) func(any) error {
	return func(any) error { return spec.HTTPError{Code: code, Contents: []byte(body)} }
}

func TestPolicyServerSign(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cfg := &config.FederationAPI{Matrix: &config.Global{
		SigningIdentity: fclient.SigningIdentity{ServerName: "test", KeyID: "ed25519:1", PrivateKey: key},
	}}
	alice := test.NewUser(t)
	room := test.NewRoom(t, alice)
	event := room.CreateEvent(t, alice, "m.room.message", map[string]any{"body": "hi"})

	const stable, unstable = policyServerSignPath, unstablePolicyServerSignPath
	for _, tc := range []struct {
		name       string
		answers    map[string]func(any) error
		wantPaths  []string
		wantSigned bool
		refusal    string // expected errcode of a refusal, if any
	}{
		{name: "stable", answers: map[string]func(any) error{stable: signed}, wantPaths: []string{stable}, wantSigned: true},
		{name: "unstable fallback", answers: map[string]func(any) error{unstable: signed}, wantPaths: []string{stable, unstable}, wantSigned: true},
		{
			name:      "refused",
			answers:   map[string]func(any) error{stable: httpError(http.StatusBadRequest, `{"errcode":"M_FORBIDDEN","error":"spam"}`)},
			wantPaths: []string{stable}, refusal: "M_FORBIDDEN",
		},
		{
			name:      "refused without errcode",
			answers:   map[string]func(any) error{stable: httpError(http.StatusForbidden, `not json`)},
			wantPaths: []string{stable}, refusal: "M_FORBIDDEN",
		},
		{name: "not a policy server", answers: map[string]func(any) error{}, wantPaths: []string{stable, unstable}},
		{name: "rate limited", answers: map[string]func(any) error{stable: httpError(http.StatusTooManyRequests, `{"errcode":"M_LIMIT_EXCEEDED"}`)}, wantPaths: []string{stable}},
		{name: "server error", answers: map[string]func(any) error{stable: httpError(http.StatusBadGateway, ``)}, wantPaths: []string{stable}},
		{name: "unreachable", answers: map[string]func(any) error{stable: func(any) error { return errors.New("connection refused") }}, wantPaths: []string{stable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &policySignClient{answers: tc.answers}
			fsAPI := &FederationInternalAPI{cfg: cfg, federation: client}
			sigs, err := fsAPI.PolicyServerSign(context.Background(), "test", "policy.example", gomatrixserverlib.PDU(event))
			require.Equal(t, tc.wantPaths, client.paths)
			var refusal *api.PolicyServerRefusal
			switch {
			case tc.wantSigned:
				require.NoError(t, err)
				require.NotEmpty(t, sigs["policy.example"]["ed25519:policy_server"])
			case tc.refusal != "":
				require.True(t, errors.As(err, &refusal), "%v", err)
				require.Equal(t, tc.refusal, refusal.ErrCode)
			default:
				require.Error(t, err)
				require.False(t, errors.As(err, &refusal), "an outage is not a refusal: %v", err)
			}
		})
	}
}
