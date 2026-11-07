// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"encoding/json"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"

	"codefloe.com/pat-s/zendrite/internal/eventutil"
	"codefloe.com/pat-s/zendrite/internal/pushrules"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/userapi/producers"
)

type recordingPublisher struct {
	msgs []*nats.Msg
}

func (p *recordingPublisher) PublishMsg(m *nats.Msg, _ ...nats.PubOpt) (*nats.PubAck, error) {
	p.msgs = append(p.msgs, m)
	return &nats.PubAck{}, nil
}

// A room upgrade copies the push rules and tags of the local members to the
// new room, and tells the sync API about it under their full user IDs.
func TestHandleRoomUpgrade(t *testing.T) {
	const oldRoomID, newRoomID = "!old:test", "!new:test"
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := mustCreateDatabase(t, dbType)
		defer closeDB()
		ctx := t.Context()

		_, err := db.UpdatePushRules(ctx, "alice", "test", func(rs *pushrules.AccountRuleSets) (bool, error) {
			rs.Global.Room = append(rs.Global.Room, &pushrules.Rule{
				RuleID: oldRoomID, Enabled: true, Actions: []*pushrules.Action{{Kind: pushrules.DontNotifyAction}},
			})
			return true, nil
		})
		assert.NoError(t, err)
		assert.NoError(t, db.SaveAccountData(ctx, "alice", "test", oldRoomID, "m.tag", json.RawMessage(`{"tags":{"u.work":{}}}`)))

		publisher := &recordingPublisher{}
		consumer := OutputRoomEventConsumer{db: db, syncProducer: producers.NewSyncAPI(db, publisher, "clientdata", "notificationdata")}
		alice := &localMembership{
			UserID: "@alice:test", Localpart: "alice", Domain: "test",
			MemberContent: gomatrixserverlib.MemberContent{Membership: spec.Join},
		}
		assert.NoError(t, consumer.handleRoomUpgrade(ctx, oldRoomID, newRoomID, []*localMembership{alice}, 3))

		rules, err := db.QueryPushRules(ctx, "alice", "test")
		assert.NoError(t, err)
		var roomRuleIDs []string
		for _, rule := range rules.Global.Room {
			roomRuleIDs = append(roomRuleIDs, rule.RuleID)
		}
		assert.Equal(t, []string{oldRoomID, newRoomID}, roomRuleIDs)

		tag, err := db.GetAccountDataByType(ctx, "alice", "test", newRoomID, "m.tag")
		assert.NoError(t, err)
		assert.JSONEq(t, `{"tags":{"u.work":{}}}`, string(tag))

		var sent []eventutil.AccountData
		for _, m := range publisher.msgs {
			assert.Equal(t, "@alice:test", m.Header.Get(jetstream.UserID))
			var data eventutil.AccountData
			assert.NoError(t, json.Unmarshal(m.Data, &data))
			sent = append(sent, data)
		}
		assert.Equal(t, []eventutil.AccountData{
			{Type: "m.push_rules"},
			{RoomID: newRoomID, Type: "m.tag"},
		}, sent)

		// Seeing the upgrade again copies nothing more.
		publisher.msgs = nil
		assert.NoError(t, consumer.handleRoomUpgrade(ctx, oldRoomID, newRoomID, []*localMembership{alice}, 3))
		rules, err = db.QueryPushRules(ctx, "alice", "test")
		assert.NoError(t, err)
		assert.Len(t, rules.Global.Room, 2)
	})
}
