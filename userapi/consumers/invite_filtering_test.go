// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"encoding/json"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/userapi/api"
	"codefloe.com/pat-s/zendrite/userapi/producers"
)

// TestInviteFilteringPush checks that invites the invitee ignores or blocks
// (MSC4155) never notify, and that allowed invites still do.
func TestInviteFilteringPush(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := mustCreateDatabase(t, dbType)
		defer closeDB()
		alice, bob := test.NewUser(t), test.NewUser(t)
		room := test.NewRoom(t, alice)
		stateKey := alice.ID
		member := synctypes.ClientEvent{Type: spec.MRoomMember, StateKey: &stateKey, Content: []byte(`{"membership":"join"}`)}

		publisher := &capturePublisher{}
		consumer := OutputRoomEventConsumer{
			cfg: &config.UserAPI{Matrix: &config.Global{ServerName: "test"}}, db: db, rsAPI: &threadRoomserver{member: member},
			syncProducer: producers.NewSyncAPI(db, publisher, "client", "notifications"),
		}
		localpart, domain, err := gomatrixserverlib.SplitID('@', bob.ID)
		require.NoError(t, err)
		setConfig := func(content string) {
			t.Helper()
			require.NoError(t, db.SaveAccountData(t.Context(), localpart, domain, "", api.MSC4155InvitePermissionConfigType, json.RawMessage(content)))
		}
		notified := func(pos uint64) bool {
			t.Helper()
			publisher.msgs = nil
			invite := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{"membership": spec.Invite}, test.WithStateKey(bob.ID))
			require.NoError(t, consumer.processMessage(t.Context(), invite, pos))
			for _, msg := range publisher.msgs {
				if msg.Subject == "notifications" && msg.Header.Get("user_id") == bob.ID {
					return true
				}
			}
			return false
		}

		require.True(t, notified(1), "an invite notifies without a configuration")

		setConfig(`{"ignored_users":["` + alice.ID + `"]}`)
		require.False(t, notified(2), "an ignored invite must not notify")

		setConfig(`{"ignored_servers":["test"]}`)
		require.False(t, notified(3), "an invite from an ignored server must not notify")

		setConfig(`{"blocked_servers":["*"]}`)
		require.False(t, notified(4), "a blocked invite must not notify")

		setConfig(`{"allowed_users":["` + alice.ID + `"],"ignored_servers":["*"]}`)
		require.True(t, notified(5), "an allowed invite notifies")

		setConfig(`{"enabled":false,"ignored_servers":["*"]}`)
		require.True(t, notified(6), "a disabled configuration filters nothing")
	})
}
