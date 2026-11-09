// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package roomserver_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

// An invite received over federation is marked NotInRoom only when this
// server is not in the room. Otherwise it also arrives as a room event, and
// the sync API must not forward it to application services a second time.
func TestHandleInviteMarksNotInRoom(t *testing.T) {
	for _, inRoom := range []bool{false, true} {
		name := "room this server is not in"
		if inRoom {
			name = "room this server is in"
		}
		t.Run(name, func(t *testing.T) {
			alice := test.NewUser(t)
			bob := test.NewUser(t)
			room := test.NewRoom(t, alice)
			ctx := context.Background()

			cfg, processCtx, closeDB := testrig.CreateConfig(t, test.DBTypeSQLite)
			defer closeDB()
			cm := sqlutil.NewConnectionManager(processCtx, cfg.Global.DatabaseOptions)
			caches := caching.NewRistrettoCache(128*1024*1024, time.Hour, caching.DisableMetrics)
			natsInstance := jetstream.NATSInstance{}
			jsctx, nc := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
			defer jetstream.DeleteAllStreams(jsctx, &cfg.Global.JetStream)
			rsAPI := roomserver.NewInternalAPI(processCtx, cfg, cm, &natsInstance, caches, caching.DisableMetrics)
			rsAPI.SetFederationAPI(nil, nil)

			if inRoom {
				require.NoError(t, api.SendEvents(ctx, rsAPI, api.KindNew, room.Events(), "test", "test", "test", nil, false))
			}
			outputs, err := nc.SubscribeSync(cfg.Global.JetStream.Prefixed(jetstream.OutputRoomEvent))
			require.NoError(t, err)

			invite := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(bob.ID))
			require.NoError(t, rsAPI.HandleInvite(ctx, invite))

			var out api.OutputEvent
			for out.Type != api.OutputTypeNewInviteEvent {
				msg, nextErr := outputs.NextMsg(5 * time.Second)
				require.NoError(t, nextErr)
				out = api.OutputEvent{}
				require.NoError(t, json.Unmarshal(msg.Data, &out))
			}
			require.Equal(t, invite.EventID(), out.NewInviteEvent.Event.EventID())
			require.Equal(t, !inRoom, out.NewInviteEvent.NotInRoom)
		})
	}
}
