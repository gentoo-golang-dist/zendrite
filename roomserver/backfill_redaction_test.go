// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	fedapi "codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/internal/perform"
	"codefloe.com/pat-s/zendrite/roomserver/storage"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// backfillServer answers a backfill with the given events.
type backfillServer struct {
	fedapi.RoomserverFederationAPI
	events   []*types.HeaderedEvent
	stateIDs []string
}

func (b *backfillServer) Backfill(
	_ context.Context, _, _ spec.ServerName, _ string, _ int, _ []string,
) (gomatrixserverlib.Transaction, error) {
	var txn gomatrixserverlib.Transaction
	for _, ev := range b.events {
		txn.PDUs = append(txn.PDUs, ev.JSON())
	}
	return txn, nil
}

func (b *backfillServer) LookupStateIDs(
	_ context.Context, _, _ spec.ServerName, _, _ string,
) (gomatrixserverlib.StateIDResponse, error) {
	return fclient.RespStateIDs{StateEventIDs: b.stateIDs, AuthEventIDs: b.stateIDs}, nil
}

// A server that joined a room after a message was redacted backfills the
// message and the redaction together, and the other server sends the message
// with its content already removed. Whether the redaction is allowed depends
// on the state at it, so it must be applied once the event has state, or the
// message is shown blank instead of as redacted.
func TestBackfillAppliesRedactionOfEventInSameBatch(t *testing.T) {
	alice := test.NewUser(t)
	ctx := context.Background()

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		cfg, processCtx, closeRig := testrig.CreateConfig(t, dbType)
		defer closeRig()
		cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
		caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
		db, err := storage.Open(processCtx.Context(), cm, &cfg.RoomServer.Database, caches) //nolint:contextcheck
		require.NoError(t, err)

		natsInstance := jetstream.NATSInstance{}
		jsCtx, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
		defer jetstream.DeleteAllStreams(jsCtx, &cfg.Global.JetStream)
		rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
		rsAPI.SetFederationAPI(nil, nil)

		room := test.NewRoom(t, alice, test.RoomVersion(gomatrixserverlib.RoomVersionV10))
		initial := room.Events()
		require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, initial, "test", "test", "test", nil, false))

		message := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "secret"})
		redaction := room.CreateAndInsert(t, alice, spec.MRoomRedaction,
			map[string]any{"redacts": message.EventID()}, test.WithRedacts(message.EventID()))

		stateIDs := make([]string, len(initial))
		for i, ev := range initial {
			stateIDs[i] = ev.EventID()
		}
		backfiller := &perform.Backfiller{
			IsLocalServerName: func(s spec.ServerName) bool { return s == "local" },
			DB:                db,
			FSAPI:             &backfillServer{events: []*types.HeaderedEvent{message, redaction}, stateIDs: stateIDs},
			KeyRing:           &test.NopJSONVerifier{},
			Querier:           rsAPI,
		}
		var res api.PerformBackfillResponse
		require.NoError(t, backfiller.PerformBackfill(ctx, &api.PerformBackfillRequest{
			RoomID:               room.ID,
			BackwardsExtremities: map[string][]string{initial[len(initial)-1].EventID(): {redaction.EventID()}},
			Limit:                100,
			ServerName:           "local",
			VirtualHost:          "local",
		}, &res))

		var returned *types.HeaderedEvent
		for _, ev := range res.Events {
			if ev.EventID() == message.EventID() {
				returned = ev
			}
		}
		require.NotNil(t, returned, "the backfill must return the message")
		require.Equal(t, redaction.EventID(), gjson.GetBytes(returned.Unsigned(), "redacted_by").Str,
			"the returned message must say what redacted it")
		require.False(t, gjson.GetBytes(returned.Content(), "body").Exists())

		require.Len(t, res.Redactions, 1)
		require.Equal(t, message.EventID(), res.Redactions[0].RedactedEventID)
		require.Equal(t, redaction.EventID(), res.Redactions[0].RedactedBecause.EventID())

		// The redaction keeps its state, as any other backfilled event.
		nid, err := db.SnapshotNIDFromEventID(ctx, redaction.EventID())
		require.NoError(t, err)
		require.NotZero(t, nid)

		// The stored message is redacted too.
		nids, err := db.EventNIDs(ctx, []string{message.EventID()})
		require.NoError(t, err)
		stored, err := db.Events(ctx, room.Version, []types.EventNID{nids[message.EventID()].EventNID})
		require.NoError(t, err)
		require.Equal(t, redaction.EventID(), gjson.GetBytes(stored[0].Unsigned(), "redacted_by").Str)
	})
}
