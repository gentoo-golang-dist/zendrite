// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sync

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/notifier"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/streams"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

func TestValidateStableExtensions(t *testing.T) {
	t.Parallel()

	toDevice := func(limit *int, since string) *types.ExtensionRequest {
		return &types.ExtensionRequest{ToDevice: &types.ToDeviceRequest{Enabled: true, Limit: limit, Since: since}}
	}
	for _, tc := range []struct {
		name    string
		ext     *types.ExtensionRequest
		errcode string
	}{
		{name: "no extensions"},
		{name: "defaults", ext: toDevice(nil, "")},
		{name: "valid", ext: toDevice(new(5), "12")},
		{name: "zero limit", ext: toDevice(new(0), ""), errcode: "M_BAD_JSON"},
		{name: "negative limit", ext: toDevice(new(-1), ""), errcode: "M_BAD_JSON"},
		{name: "unknown since", ext: toDevice(nil, "s1_2"), errcode: "M_INVALID_PARAM"},
		{name: "negative since", ext: toDevice(nil, "-3"), errcode: "M_INVALID_PARAM"},
		{name: "disabled", ext: &types.ExtensionRequest{ToDevice: &types.ToDeviceRequest{Limit: new(0)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := validateStableExtensions(tc.ext)
			if tc.errcode == "" {
				assert.Nil(t, res)
				return
			}
			require.NotNil(t, res)
			assert.Equal(t, http.StatusBadRequest, res.Code)
			matrixErr, ok := res.JSON.(spec.MatrixError)
			require.True(t, ok, "%T", res.JSON)
			assert.Equal(t, tc.errcode, string(matrixErr.ErrCode))
		})
	}
}

func TestKeyChangeToken(t *testing.T) {
	t.Parallel()

	streamToken := types.StreamingToken{DeviceListPosition: 7}
	got, err := keyChangeToken(streamToken.String())
	require.NoError(t, err)
	assert.Equal(t, streamToken, got)

	// A sliding sync pos (MSC4543) carries the stream token it was issued at.
	got, err = keyChangeToken(types.NewSlidingSyncStreamToken(3, streamToken).String())
	require.NoError(t, err)
	assert.Equal(t, streamToken, got)

	_, err = keyChangeToken("nonsense")
	assert.Error(t, err)
}

// TestTypingCurrentStateForRoomsInResponse checks that a room the response
// sends, which may just have entered scope, carries its current typing state
// even if it did not change since pos (MSC4508).
func TestTypingCurrentStateForRoomsInResponse(t *testing.T) {
	cache := caching.NewTypingCache(t.Context())
	cache.AddTypingUser("@bob:test", "!entered:test", nil)
	cache.AddTypingUser("@bob:test", "!stopped:test", nil)
	cache.RemoveUser("@bob:test", "!stopped:test")
	pos := cache.AddTypingUser("@bob:test", "!stale:test", nil)
	pool := RequestPool{streams: &streams.Streams{TypingStreamProvider: &streams.TypingStreamProvider{EDUCache: cache}}}
	subscriptions := map[string]bool{"!entered:test": true, "!stopped:test": true, "!stale:test": true}
	fromPos := &types.StreamingToken{TypingPosition: types.StreamPosition(pos)}
	responseRooms := map[string]types.SlidingRoomData{"!entered:test": {}, "!stopped:test": {}}

	response, err := pool.processTypingExtension(t.Context(), joinedTo("!entered:test", "!stopped:test", "!stale:test"), "@alice:test", nil, nil, fromPos, types.StreamingToken{}, nil, subscriptions, responseRooms)
	require.NoError(t, err)
	assert.Contains(t, response.Rooms, "!entered:test", "a room in the response gets its current typing state")
	assert.JSONEq(t, `{"user_ids":[]}`, string(response.Rooms["!stopped:test"].Content), "including that nobody types any more")
	assert.NotContains(t, response.Rooms, "!stale:test", "other rooms only get changes after pos")
}

// TestStableSlidingSyncToDeviceEndToEnd sends to_device requests to both
// sliding sync endpoints: /v4/sync reads the MSC4538 prefixed key, validates
// limit and answers in the MSC's format under the same key, while the
// org.matrix.simplified_msc3575 endpoint ignores the prefixed key.
func TestStableSlidingSyncToDeviceEndToEnd(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		db, err := storage.NewSyncServerDatasource(processCtx.Context(), cm, &cfg.SyncAPI.Database)
		require.NoError(t, err)
		ctx := t.Context()
		device := &userapi.Device{UserID: "@alice:localhost", ID: "DEVICE"}

		pos, err := db.StoreNewSendForDeviceMessage(ctx, device.UserID, device.ID, gomatrixserverlib.SendToDeviceEvent{
			Sender: "@bob:localhost", Type: "m.room_key", Content: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
		rp := &RequestPool{
			db:       db,
			cfg:      &config.SyncAPI{Matrix: &config.Global{}},
			lastseen: &sync.Map{},
			presence: &sync.Map{},
			Notifier: notifier.NewNotifier(nil),
			rsAPI:    noPartialStateRoomserver{},
		}
		rp.Notifier.SetCurrentPosition(types.StreamingToken{SendToDevicePosition: pos})
		rp.lastseen.Store(device.UserID+device.ID, struct{}{})

		do := func(stable bool, toDevice map[string]any) (int, []byte) {
			body, marshalErr := json.Marshal(map[string]any{
				"conn_id":    fmt.Sprintf("%v", stable),
				"extensions": map[string]any{"org.matrix.msc4538.to_device": toDevice},
			})
			require.NoError(t, marshalErr)
			req := httptest.NewRequest(http.MethodPost, "/_matrix/client/v4/sync", strings.NewReader(string(body)))
			var res util.JSONResponse
			if stable {
				res = rp.OnIncomingStableSyncRequestV4(req, device)
			} else {
				res = rp.OnIncomingSyncRequestV4(req, device)
			}
			out, marshalErr := json.Marshal(res.JSON)
			require.NoError(t, marshalErr)
			return res.Code, out
		}

		code, out := do(true, map[string]any{"enabled": true, "limit": 0})
		require.Equal(t, http.StatusBadRequest, code, string(out))
		assert.Equal(t, "M_BAD_JSON", gjson.GetBytes(out, "errcode").Str)

		code, out = do(true, map[string]any{"enabled": true})
		require.Equal(t, http.StatusOK, code, string(out))
		section := gjson.GetBytes(out, `extensions.org\.matrix\.msc4538\.to_device`)
		require.True(t, section.Exists(), string(out))
		assert.Len(t, section.Get("messages").Array(), 1)
		assert.False(t, section.Get("events").Exists())

		code, out = do(false, map[string]any{"enabled": true})
		require.Equal(t, http.StatusOK, code, string(out))
		assert.False(t, gjson.GetBytes(out, `extensions.org\.matrix\.msc4538\.to_device`).Exists(), string(out))
		assert.False(t, gjson.GetBytes(out, "extensions.to_device").Exists(), string(out))
	})
}
