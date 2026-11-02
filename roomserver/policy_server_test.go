// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	fedapi "codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

const policyServerName spec.ServerName = "policy.example"

// fakePolicyServer answers /sign requests (MSC4284) as configured.
type fakePolicyServer struct {
	fedapi.RoomserverFederationAPI
	key ed25519.PrivateKey

	mu    sync.Mutex
	mode  string // "sign", "refuse", "empty", "down" or "badsig"
	calls int
}

func (f *fakePolicyServer) set(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode, f.calls = mode, 0
}

func (f *fakePolicyServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakePolicyServer) PolicyServerSign(_ context.Context, origin, policyServer spec.ServerName, event gomatrixserverlib.PDU) (map[string]map[string]spec.Base64Bytes, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if policyServer != policyServerName || origin != "test" {
		return nil, errors.New("unexpected request")
	}
	switch f.mode {
	case "refuse":
		return nil, &fedapi.PolicyServerRefusal{StatusCode: http.StatusBadRequest, ErrCode: "M_FORBIDDEN", Message: "Too many mentions"}
	case "empty":
		return map[string]map[string]spec.Base64Bytes{}, nil
	case "down":
		return nil, errors.New("connection refused")
	}
	key := f.key
	if f.mode == "badsig" {
		_, key, _ = ed25519.GenerateKey(rand.Reader)
	}
	return map[string]map[string]spec.Base64Bytes{
		string(policyServerName): {"ed25519:policy_server": policySignature(key, event)},
	}, nil
}

// policySignature signs the event like a policy server.
func policySignature(key ed25519.PrivateKey, event gomatrixserverlib.PDU) spec.Base64Bytes {
	verImpl := gomatrixserverlib.MustGetRoomVersion(event.Version())
	redacted, err := verImpl.RedactEventJSON(event.JSON())
	if err != nil {
		panic(err)
	}
	signed, err := gomatrixserverlib.SignJSON(string(policyServerName), "ed25519:policy_server", key, redacted)
	if err != nil {
		panic(err)
	}
	var sig spec.Base64Bytes
	if err = sig.Decode(gjson.GetBytes(signed, "signatures."+gjson.Escape(string(policyServerName))+`.ed25519:policy_server`).Str); err != nil {
		panic(err)
	}
	return sig
}

// TestPolicyServer covers checking events with a room's policy server
// (MSC4284): local events are signed or refused, remote events without a
// valid signature are soft failed, and outages fail closed.
func TestPolicyServer(t *testing.T) {
	alice := test.NewUser(t)
	_, botKey, _ := ed25519.GenerateKey(rand.Reader)
	bot := test.NewUser(t, test.WithSigningServer(policyServerName, "ed25519:bot", botKey))
	policyPublic, policyKey, _ := ed25519.GenerateKey(rand.Reader)
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeDB := testrig.CreateConfig(t, dbType)
		defer closeDB()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		natsInstance := jetstream.NATSInstance{}
		jsCtx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsCtx, &cfg.Global.JetStream)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		policy := &fakePolicyServer{key: policyKey, mode: "sign"}
		rsAPI.SetFederationAPI(policy, nil)

		room := test.NewRoom(t, alice, test.RoomPreset(test.PresetPublicChat))
		room.CreateAndInsert(t, bot, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bot.ID))
		policyContent := map[string]any{
			"via":         policyServerName,
			"public_keys": map[string]any{"ed25519": spec.Base64Bytes(policyPublic).Encode()},
		}
		room.CreateAndInsert(t, alice, api.MRoomPolicy, policyContent, test.WithStateKey(""))
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
		require.Zero(t, policy.callCount(), "the policy event and earlier events are not checked")

		send := func(event *types.HeaderedEvent, origin spec.ServerName) error {
			t.Helper()
			sendAs := origin
			if origin != "test" {
				sendAs = ""
			}
			return api.SendEvents(ctx, rsAPI, api.KindNew, []*types.HeaderedEvent{event}, "test", origin, sendAs, nil, false)
		}
		stored := func(eventID string) *types.HeaderedEvent {
			t.Helper()
			res := api.QueryEventsByIDResponse{}
			require.NoError(t, rsAPI.QueryEventsByID(ctx, &api.QueryEventsByIDRequest{RoomID: room.ID, EventIDs: []string{eventID}}, &res))
			if len(res.Events) == 0 {
				return nil
			}
			return res.Events[0]
		}
		latest := func() []string {
			t.Helper()
			res := api.QueryLatestEventsAndStateResponse{}
			require.NoError(t, rsAPI.QueryLatestEventsAndState(ctx, &api.QueryLatestEventsAndStateRequest{RoomID: room.ID}, &res))
			return res.LatestEvents
		}
		message := func(sender *test.User, body string) *types.HeaderedEvent {
			return room.CreateEvent(t, sender, "m.room.message", map[string]any{"msgtype": "m.text", "body": body})
		}
		hasPolicySignature := func(event *types.HeaderedEvent) bool {
			return gjson.GetBytes(event.JSON(), "signatures."+gjson.Escape(string(policyServerName))+`.ed25519:policy_server`).Exists()
		}

		t.Run("local event is signed", func(t *testing.T) {
			policy.set("sign")
			event := message(alice, "hello")
			require.NoError(t, send(event, "test"))
			require.Equal(t, 1, policy.callCount())
			got := stored(event.EventID())
			require.NotNil(t, got)
			require.True(t, hasPolicySignature(got), string(got.JSON()))
			room.InsertEvent(t, got)
		})

		for _, tc := range []struct {
			mode, errCode string
			status        int
		}{
			{"refuse", "M_FORBIDDEN", http.StatusBadRequest},
			{"empty", "M_FORBIDDEN", http.StatusBadRequest},
			{"down", "M_UNKNOWN", http.StatusServiceUnavailable},
			{"badsig", "M_UNKNOWN", http.StatusServiceUnavailable},
		} {
			t.Run("local event, policy server "+tc.mode, func(t *testing.T) {
				policy.set(tc.mode)
				event := message(alice, "spam "+tc.mode)
				err := send(event, "test")
				res, ok := api.PolicyServerRejection(err)
				require.True(t, ok, "%v", err)
				require.Equal(t, tc.status, res.Code)
				matrixErr, isMatrixErr := res.JSON.(spec.MatrixError)
				require.True(t, isMatrixErr)
				require.Equal(t, tc.errCode, string(matrixErr.ErrCode))
				require.Nil(t, stored(event.EventID()), "a refused local event is not stored")
			})
		}

		t.Run("remote event with a valid signature", func(t *testing.T) {
			policy.set("refuse")
			event := message(bot, "signed elsewhere")
			signed := event.Sign(string(policyServerName), "ed25519:bot", botKey)
			withSig := addSignature(t, signed, policySignature(policyKey, signed))
			require.NoError(t, send(withSig, policyServerName))
			require.Zero(t, policy.callCount(), "a valid signature needs no request")
			require.Contains(t, latest(), withSig.EventID())
			room.InsertEvent(t, withSig)
		})

		t.Run("remote event without a signature is checked", func(t *testing.T) {
			policy.set("sign")
			event := message(bot, "unsigned but fine")
			require.NoError(t, send(event, policyServerName))
			require.Equal(t, 1, policy.callCount())
			got := stored(event.EventID())
			require.True(t, hasPolicySignature(got), "the signature is kept with the event")
			require.Contains(t, latest(), event.EventID())
			room.InsertEvent(t, got)
		})

		for _, mode := range []string{"refuse", "down"} {
			t.Run("remote event soft failed, policy server "+mode, func(t *testing.T) {
				policy.set(mode)
				event := message(bot, "remote spam "+mode)
				_, ok := api.PolicyServerRejection(send(event, policyServerName))
				require.True(t, ok)
				require.NotNil(t, stored(event.EventID()), "a soft-failed event is stored")
				require.NotContains(t, latest(), event.EventID())
			})
		}

		t.Run("local users can always leave", func(t *testing.T) {
			carol := test.NewUser(t)
			policy.set("sign")
			join := room.CreateEvent(t, carol, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(carol.ID))
			require.NoError(t, send(join, "test"))
			room.InsertEvent(t, stored(join.EventID()))

			for _, mode := range []string{"down", "refuse"} {
				policy.set(mode)
				// Another local event is still refused.
				_, refused := api.PolicyServerRejection(send(message(carol, "spam"), "test"))
				require.True(t, refused)
			}
			// A reason could carry refused content, so such a leave is checked.
			withReason := room.CreateEvent(t, carol, spec.MRoomMember, map[string]any{"membership": spec.Leave, "reason": "spam"}, test.WithStateKey(carol.ID))
			_, refused := api.PolicyServerRejection(send(withReason, "test"))
			require.True(t, refused)
			leave := room.CreateEvent(t, carol, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(carol.ID))
			require.NoError(t, send(leave, "test"))
			require.Contains(t, latest(), leave.EventID())
			room.InsertEvent(t, stored(leave.EventID()))
		})

		t.Run("a policy server that left the room is not asked", func(t *testing.T) {
			policy.set("sign")
			leave := room.CreateEvent(t, bot, spec.MRoomMember, map[string]any{"membership": spec.Leave}, test.WithStateKey(bot.ID))
			require.NoError(t, send(leave, policyServerName))
			require.Equal(t, 1, policy.callCount(), "the leave itself is still checked")
			room.InsertEvent(t, stored(leave.EventID()))

			policy.set("down")
			event := message(alice, "policy server gone")
			require.NoError(t, send(event, "test"))
			require.Zero(t, policy.callCount())
			room.InsertEvent(t, stored(event.EventID()))
		})

		t.Run("unsetting the policy server is never checked", func(t *testing.T) {
			policy.set("down")
			unset := room.CreateAndInsert(t, alice, api.MRoomPolicy, map[string]any{}, test.WithStateKey(""))
			require.NoError(t, send(unset, "test"))
			require.Zero(t, policy.callCount())
			event := message(alice, "no policy server")
			require.NoError(t, send(event, "test"))
			require.Zero(t, policy.callCount())
			require.True(t, slices.Contains(latest(), event.EventID()))
		})
	})
}

// addSignature adds the policy server's signature to the event JSON.
func addSignature(t *testing.T, event gomatrixserverlib.PDU, sig spec.Base64Bytes) *types.HeaderedEvent {
	t.Helper()
	var eventJSON map[string]any
	require.NoError(t, json.Unmarshal(event.JSON(), &eventJSON))
	signatures, ok := eventJSON["signatures"].(map[string]any)
	require.True(t, ok)
	server, _ := signatures[string(policyServerName)].(map[string]any)
	if server == nil {
		server = map[string]any{}
	}
	server["ed25519:policy_server"] = sig.Encode()
	signatures[string(policyServerName)] = server
	raw, err := json.Marshal(eventJSON)
	require.NoError(t, err)
	raw, err = gomatrixserverlib.EnforcedCanonicalJSON(raw, event.Version())
	require.NoError(t, err)
	pdu, err := gomatrixserverlib.MustGetRoomVersion(event.Version()).NewEventFromTrustedJSONWithEventID(event.EventID(), raw, false)
	require.NoError(t, err)
	return &types.HeaderedEvent{PDU: pdu}
}
