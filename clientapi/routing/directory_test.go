// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// A failed lookup on a remote server is not a fault of this server, so it
// must never surface as a 500.
func TestDirectoryRoomRemoteErrors(t *testing.T) {
	var cfg config.Zendrite
	cfg.Defaults(config.DefaultOpts{Generate: true, SingleDatabase: true})
	cfg.Global.ServerName = "local.test"

	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{name: "remote not found", err: spec.HTTPError{Code: http.StatusNotFound}, code: http.StatusNotFound},
		{name: "remote server error", err: spec.HTTPError{Code: http.StatusInternalServerError}, code: http.StatusBadGateway},
		{name: "unreachable", err: errors.New("dial tcp: connect: network is unreachable"), code: http.StatusBadGateway},
		{name: "network timeout", err: timeoutError{}, code: http.StatusGatewayTimeout},
		{name: "deadline exceeded", err: context.DeadlineExceeded, code: http.StatusGatewayTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/directory/room/%23remote:remote.test", nil)
			fed := &aliasFederation{t: t, err: tc.err}
			res := DirectoryRoom(req, "#remote:remote.test", fed, &cfg.ClientAPI, &aliasRoomserver{}, nil)
			require.Equal(t, tc.code, res.Code)
		})
	}
}
