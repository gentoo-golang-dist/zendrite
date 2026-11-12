package base

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// The legacy media fallback lets any local user point CreateClient's client at
// an arbitrary server name, so it must refuse to dial the denied networks.
func TestCreateClientAppliesDenyNetworks(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	serverName := spec.ServerName(u.Host) // 127.0.0.1:<port>

	newCfg := func() *config.Zendrite {
		cfg := &config.Zendrite{}
		cfg.Defaults(config.DefaultOpts{Generate: true, SingleDatabase: true})
		cfg.FederationAPI.DisableTLSValidation = true
		return cfg
	}
	download := func(cfg *config.Zendrite) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := CreateClient(cfg, nil).CreateMediaDownloadRequest(ctx, serverName, "media")
		if resp != nil {
			_ = resp.Body.Close()
		}
		return err
	}

	// The default deny list covers loopback, so the server is never contacted.
	require.Error(t, download(newCfg()))
	require.Zero(t, hits.Load(), "denied address was contacted")

	// Control: with loopback no longer denied the same request goes through,
	// proving the failure above came from the network restriction.
	cfg := newCfg()
	cfg.FederationAPI.DenyNetworkCIDRs = nil
	require.NoError(t, download(cfg))
	require.EqualValues(t, 1, hits.Load())
}

// Identity servers are configured by the operator and may be on a private
// network, so the identity client keeps ignoring the federation network lists.
func TestCreateIdentityClientIgnoresDenyNetworks(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Zendrite{}
	cfg.Defaults(config.DefaultOpts{Generate: true, SingleDatabase: true})
	cfg.FederationAPI.DisableTLSValidation = true

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	resp, err := CreateIdentityClient(cfg).DoHTTPRequest(ctx, req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.EqualValues(t, 1, hits.Load())
}
