// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/userapi/api"
	"codefloe.com/pat-s/zendrite/userapi/storage"
)

// fakeKeyAppservice answers the MSC3983 and MSC3984 endpoints and records
// what it was asked.
type fakeKeyAppservice struct {
	mu      sync.Mutex
	claims  []map[string]map[string][]string
	queries []map[string][]string
	tokens  []string
	claim   string
	query   string
}

func (f *fakeKeyAppservice) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer hs_token" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	f.tokens = append(f.tokens, r.URL.Query().Get("access_token"))
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Path {
	case appserviceKeysClaimPath:
		var req map[string]map[string][]string
		_ = json.Unmarshal(body, &req)
		f.claims = append(f.claims, req)
		_, _ = w.Write([]byte(f.claim))
	case appserviceKeysQueryPath:
		var req map[string][]string
		_ = json.Unmarshal(body, &req)
		f.queries = append(f.queries, req)
		_, _ = w.Write([]byte(f.query))
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errcode":"M_UNRECOGNIZED"}`))
	}
}

func newAppserviceKeysAPI(t *testing.T, dbType test.DBType, url string) (*UserInternalAPI, func()) {
	t.Helper()
	connStr, closeDB := test.PrepareDBConnectionString(t, dbType)
	cm := sqlutil.NewConnectionManager(nil, config.DatabaseOptions{})
	db, err := storage.NewKeyDatabase(cm, &config.DatabaseOptions{ConnectionString: config.DataSource(connStr)})
	require.NoError(t, err)
	return &UserInternalAPI{
		KeyDatabase: db,
		Config:      &config.UserAPI{Matrix: &config.Global{ServerName: "test"}},
		AppServices: []config.ApplicationService{{
			ID: "bridge", URL: url, HSToken: "hs_token",
			NamespaceMap: map[string][]config.ApplicationServiceNamespace{
				"users": {{Exclusive: true, Regex: "@bridge_.*:test", RegexpObject: regexp.MustCompile("@bridge_.*:test")}},
			},
		}},
		AppserviceOneTimeKeyClaims: true,
		AppserviceKeyQueries:       true,
		AppserviceLegacyAuth:       true,
	}, closeDB
}

// MSC3983: a device of an application service user without a stored
// one-time key gets one from the application service, and its fallback key
// only if the application service has none.
func TestClaimAppserviceKeys(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		as := &fakeKeyAppservice{claim: `{"@bridge_a:test":{"FROM_AS":{"signed_curve25519:AS1":{"key":"as"},"other:AS2":{"key":"wrong algorithm"}}}}`}
		srv := httptest.NewServer(as)
		defer srv.Close()
		a, closeDB := newAppserviceKeysAPI(t, dbType, srv.URL)
		defer closeDB()
		ctx := t.Context()

		key := func(value string) map[string]json.RawMessage {
			return map[string]json.RawMessage{"signed_curve25519:" + value: json.RawMessage(`{"key":"` + value + `"}`)}
		}
		_, err := a.KeyDatabase.StoreOneTimeKeys(ctx, api.OneTimeKeys{UserID: "@bridge_a:test", DeviceID: "STORED", KeyJSON: key("stored")})
		require.NoError(t, err)
		_, err = a.KeyDatabase.StoreFallbackKeys(ctx, api.FallbackKeys{UserID: "@bridge_a:test", DeviceID: "FALLBACK", KeyJSON: key("fallback")})
		require.NoError(t, err)
		_, err = a.KeyDatabase.StoreFallbackKeys(ctx, api.FallbackKeys{UserID: "@alice:test", DeviceID: "ALICE", KeyJSON: key("alice")})
		require.NoError(t, err)

		var res api.PerformClaimKeysResponse
		a.PerformClaimKeys(ctx, &api.PerformClaimKeysRequest{OneTimeKeys: map[string]map[string]string{
			"@bridge_a:test": {"STORED": "signed_curve25519", "FROM_AS": "signed_curve25519", "FALLBACK": "signed_curve25519"},
			"@alice:test":    {"ALICE": "signed_curve25519"},
		}}, &res)
		require.Nil(t, res.Error)
		require.Equal(t, map[string]map[string]map[string]json.RawMessage{
			"@bridge_a:test": {
				"STORED":   key("stored"),
				"FROM_AS":  {"signed_curve25519:AS1": json.RawMessage(`{"key":"as"}`)},
				"FALLBACK": key("fallback"),
			},
			"@alice:test": {"ALICE": key("alice")},
		}, res.OneTimeKeys)
		// Only the devices without a stored one-time key were asked for.
		require.Equal(t, []map[string]map[string][]string{{"@bridge_a:test": {
			"FROM_AS": {"signed_curve25519"}, "FALLBACK": {"signed_curve25519"},
		}}}, as.claims)
		// With legacy_auth, the token is in the query string as well.
		require.Equal(t, []string{"hs_token"}, as.tokens)

		// A malformed answer gives no keys at all.
		as.claim = `{"@bridge_a:test":{"FROM_AS":{"signed_curve25519:AS3":{"key":"as"}},"FALLBACK":"oops"}}`
		res = api.PerformClaimKeysResponse{}
		a.PerformClaimKeys(ctx, &api.PerformClaimKeysRequest{OneTimeKeys: map[string]map[string]string{
			"@bridge_a:test": {"FROM_AS": "signed_curve25519"},
		}}, &res)
		require.Empty(t, res.OneTimeKeys["@bridge_a:test"])

		// Without the MSC, the application service is not asked.
		a.AppserviceOneTimeKeyClaims = false
		a.PerformClaimKeys(ctx, &api.PerformClaimKeysRequest{OneTimeKeys: map[string]map[string]string{
			"@bridge_a:test": {"FROM_AS": "signed_curve25519"},
		}}, &res)
		require.Len(t, as.claims, 2)
	})
}

// MSC3984: what the application service answers is laid over the keys the
// server holds, device by device.
func TestQueryAppserviceKeys(t *testing.T) {
	as := &fakeKeyAppservice{query: `{
		"device_keys":{
			"@bridge_a:test":{
				"AS_DEVICE":{"user_id":"@bridge_a:test","device_id":"AS_DEVICE"},
				"SHARED":{"user_id":"@bridge_a:test","device_id":"SHARED","from":"as"},
				"WRONG_ID":{"user_id":"@bridge_a:test","device_id":"OTHER"},
				"NOT_AN_OBJECT":"null"
			},
			"@bridge_b:test":{"X":{"user_id":"@bridge_b:test","device_id":"X"}}
		},
		"master_keys":{"@bridge_a:test":{"user_id":"@bridge_a:test","usage":["master"],"keys":{"ed25519:bWFzdGVy":"bWFzdGVy"}}},
		"user_signing_keys":{"@bridge_a:test":{"user_id":"@bridge_a:test","usage":["user_signing"],"keys":{"ed25519:dXNlcg":"dXNlcg"}}}
	}`}
	srv := httptest.NewServer(as)
	defer srv.Close()
	a := &UserInternalAPI{
		Config: &config.UserAPI{Matrix: &config.Global{ServerName: "test"}},
		AppServices: []config.ApplicationService{{
			ID: "bridge", URL: srv.URL, HSToken: "hs_token",
			NamespaceMap: map[string][]config.ApplicationServiceNamespace{
				"users": {{Exclusive: true, RegexpObject: regexp.MustCompile("@bridge_.*:test")}},
			},
		}},
	}
	res := &api.QueryKeysResponse{
		DeviceKeys: map[string]map[string]json.RawMessage{"@bridge_a:test": {
			"STORED": json.RawMessage(`{"device_id":"STORED"}`),
			"SHARED": json.RawMessage(`{"device_id":"SHARED","from":"server","unsigned":{"device_display_name":"Phone"}}`),
		}},
		MasterKeys: map[string]fclient.CrossSigningKey{}, SelfSigningKeys: map[string]fclient.CrossSigningKey{}, UserSigningKeys: map[string]fclient.CrossSigningKey{},
	}
	a.queryAppserviceKeys(t.Context(), &api.QueryKeysRequest{
		UserID:        "@alice:test",
		UserToDevices: map[string][]string{"@bridge_a:test": nil, "@alice:test": nil, "@bridge_c:remote": nil},
	}, res)

	require.Equal(t, []map[string][]string{{"@bridge_a:test": {}}}, as.queries)
	require.Equal(t, map[string]json.RawMessage{
		"STORED":    json.RawMessage(`{"device_id":"STORED"}`),
		"SHARED":    json.RawMessage(`{"user_id":"@bridge_a:test","device_id":"SHARED","from":"as","unsigned":{"device_display_name":"Phone"}}`),
		"AS_DEVICE": json.RawMessage(`{"user_id":"@bridge_a:test","device_id":"AS_DEVICE"}`),
	}, res.DeviceKeys["@bridge_a:test"])
	require.NotContains(t, res.DeviceKeys, "@bridge_b:test", "a user that was not asked for")
	require.Contains(t, res.MasterKeys, "@bridge_a:test")
	require.NotContains(t, res.UserSigningKeys, "@bridge_a:test", "another user's user-signing key")

	// An application service that fails changes nothing.
	srv.Close()
	before := len(res.DeviceKeys["@bridge_a:test"])
	a.queryAppserviceKeys(t.Context(), &api.QueryKeysRequest{UserToDevices: map[string][]string{"@bridge_a:test": {"STORED"}}}, res)
	require.Len(t, res.DeviceKeys["@bridge_a:test"], before)
}
