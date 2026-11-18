// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// liveKitFederation answers the token request on each federation path with
// a response or an error, and records the paths asked.
type liveKitFederation struct {
	fclient.FederationClient
	answers map[string]any
	asked   []string
	body    map[string]any
}

func (f *liveKitFederation) DoRequestAndParseResponse(_ context.Context, req *http.Request, result any) error {
	f.asked = append(f.asked, req.URL.Path)
	_ = json.NewDecoder(req.Body).Decode(&f.body)
	switch answer := f.answers[req.URL.Path].(type) {
	case error:
		return answer
	case string:
		return json.Unmarshal([]byte(answer), result)
	}
	return spec.HTTPError{Code: http.StatusNotFound, Contents: []byte(`{"errcode":"M_UNRECOGNIZED"}`)}
}

func TestRemoteLiveKitToken(t *testing.T) {
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	cfg := &config.ClientAPI{Matrix: &config.Global{SigningIdentity: fclient.SigningIdentity{ServerName: "local", KeyID: "ed25519:a", PrivateKey: key}}}
	body := liveKitTokenRequest{UserID: "@alice:local", URL: "wss://sfu.remote", RoomID: "!r:remote", SlotID: "s", MemberID: "m"}
	stable, unstable := liveKitTokenFederationPaths[0], liveKitTokenFederationPaths[1]

	t.Run("token", func(t *testing.T) {
		fed := &liveKitFederation{answers: map[string]any{stable: `{"jwt":"token"}`}}
		res := remoteLiveKitToken(t.Context(), cfg, fed, "remote", body)
		require.Equal(t, http.StatusOK, res.Code)
		require.Equal(t, map[string]string{"jwt": "token"}, res.JSON)
		require.Equal(t, "@alice:local", fed.body["user_id"])
		require.NotContains(t, fed.body, "server_name")
	})
	t.Run("the unstable path for older servers", func(t *testing.T) {
		fed := &liveKitFederation{answers: map[string]any{unstable: `{"jwt":"token"}`}}
		res := remoteLiveKitToken(t.Context(), cfg, fed, "remote", body)
		require.Equal(t, http.StatusOK, res.Code)
		require.Equal(t, []string{stable, unstable}, fed.asked)

		// Some servers answer an unknown path with 400 M_UNRECOGNIZED.
		fed = &liveKitFederation{answers: map[string]any{
			stable:   spec.HTTPError{Code: http.StatusBadRequest, Contents: []byte(`{"errcode":"M_UNRECOGNIZED"}`)},
			unstable: `{"jwt":"token"}`,
		}}
		res = remoteLiveKitToken(t.Context(), cfg, fed, "remote", body)
		require.Equal(t, http.StatusOK, res.Code)
	})
	t.Run("errors the client is told", func(t *testing.T) {
		for code, errcode := range map[int]string{http.StatusForbidden: "M_FORBIDDEN", http.StatusBadRequest: "M_INVALID_PARAM"} {
			fed := &liveKitFederation{answers: map[string]any{stable: spec.HTTPError{Code: code, Contents: []byte(`{"errcode":"` + errcode + `","error":"no"}`)}}}
			res := remoteLiveKitToken(t.Context(), cfg, fed, "remote", body)
			require.Equal(t, code, res.Code)
			matrixErr, ok := res.JSON.(spec.MatrixError)
			require.True(t, ok)
			require.EqualValues(t, errcode, matrixErr.ErrCode)
		}
	})
	t.Run("other failures", func(t *testing.T) {
		for _, answer := range []any{
			spec.HTTPError{Code: http.StatusInternalServerError, Contents: []byte(`{"errcode":"M_UNKNOWN"}`)},
			errors.New("unreachable"),
			`{}`,
		} {
			fed := &liveKitFederation{answers: map[string]any{stable: answer}}
			res := remoteLiveKitToken(t.Context(), cfg, fed, "remote", body)
			require.Equal(t, http.StatusBadGateway, res.Code)
		}
		// Neither path is known.
		res := remoteLiveKitToken(t.Context(), cfg, &liveKitFederation{}, "remote", body)
		require.Equal(t, http.StatusBadGateway, res.Code)
	})
}
