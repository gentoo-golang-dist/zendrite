// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package clientapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	"codefloe.com/pat-s/zendrite/userapi"
)

// The name and topic keys of /createRoom override the same events in
// initial_state, as the spec says.
func TestCreateRoomNameAndTopicOverrideInitialState(t *testing.T) {
	alice := test.NewUser(t)
	cfg, processCtx, closeDB := testrig.CreateConfig(t, test.DBTypeSQLite)
	defer closeDB()
	routers := httputil.NewRouters()
	cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
	caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
	natsInstance := jetstream.NATSInstance{}
	jsctx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
	defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)
	rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
	rsAPI.SetFederationAPI(nil, nil)
	userAPI := userapi.NewInternalAPI(processCtx, cfg, cm, &natsInstance, rsAPI, nil, caching.DisableMetrics, testIsBlacklistedOrBackingOff)
	AddPublicRoutes(processCtx, routers, cfg, &natsInstance, nil, rsAPI, nil, nil, nil, userAPI, nil, nil, nil, caching.DisableMetrics)
	accessTokens := map[*test.User]userDevice{alice: {}}
	createAccessTokens(t, accessTokens, userAPI, processCtx.Context(), routers)

	initialState := []map[string]any{
		{"type": spec.MRoomName, "state_key": "", "content": map[string]any{"name": "initial name"}},
		{"type": spec.MRoomTopic, "state_key": "", "content": map[string]any{"topic": "initial topic"}},
	}
	tests := map[string]struct {
		body      map[string]any
		wantName  string
		wantTopic string
	}{
		"initial_state only": {
			body:      map[string]any{"initial_state": initialState},
			wantName:  "initial name",
			wantTopic: "initial topic",
		},
		"keys override initial_state": {
			body:      map[string]any{"initial_state": initialState, "name": "key name", "topic": "key topic"},
			wantName:  "key name",
			wantTopic: "key topic",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			routers.Client.ServeHTTP(rec, test.NewRequest(t, http.MethodPost, "/_matrix/client/v3/createRoom",
				test.WithJSONBody(t, tc.body),
				test.WithQueryParams(map[string]string{"access_token": accessTokens[alice].accessToken})))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			roomID, err := spec.NewRoomID(gjson.GetBytes(rec.Body.Bytes(), "room_id").Str)
			require.NoError(t, err)
			nameEv, err := rsAPI.CurrentStateEvent(context.Background(), *roomID, spec.MRoomName, "")
			require.NoError(t, err)
			require.NotNil(t, nameEv)
			assert.Equal(t, tc.wantName, gjson.GetBytes(nameEv.Content(), "name").Str)
			topicEv, err := rsAPI.CurrentStateEvent(context.Background(), *roomID, spec.MRoomTopic, "")
			require.NoError(t, err)
			require.NotNil(t, topicEv)
			assert.Equal(t, tc.wantTopic, gjson.GetBytes(topicEv.Content(), "topic").Str)
		})
	}
}
