// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/pushrules"
	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/userapi/storage"
)

// relatedRoomserverAPI serves stored events by ID, whatever room is asked
// for, and counts the lookups.
type relatedRoomserverAPI struct {
	FakeUserRoomserverAPI
	events  map[string]*types.HeaderedEvent
	lookups int
}

func (r *relatedRoomserverAPI) QueryEventsByID(_ context.Context, req *rsapi.QueryEventsByIDRequest, res *rsapi.QueryEventsByIDResponse) error {
	r.lookups++
	for _, id := range req.EventIDs {
		if event, ok := r.events[id]; ok {
			res.Events = append(res.Events, event)
		}
	}
	return nil
}

// MSC3664: the related events are the m.relates_to target under its rel_type
// and the m.in_reply_to target, as a fallback for a thread's reply fallback.
func TestRelatedEvents(t *testing.T) {
	original := mustCreateEvent(t, `{"type":"m.room.message","room_id":"!room:localhost","sender":"@test:localhost","content":{"body":"hi"}}`)
	root := mustCreateEvent(t, `{"type":"m.room.message","room_id":"!room:localhost","sender":"@other:localhost","content":{"body":"root"}}`)
	elsewhere := mustCreateEvent(t, `{"type":"m.room.message","room_id":"!other:localhost","sender":"@test:localhost","content":{"body":"elsewhere"}}`)
	rs := &relatedRoomserverAPI{events: map[string]*types.HeaderedEvent{
		original.EventID(): original, root.EventID(): root, elsewhere.EventID(): elsewhere,
	}}
	event := func(relatesTo string) *types.HeaderedEvent {
		return mustCreateEvent(t, `{"type":"m.room.message","room_id":"!room:localhost","sender":"@bob:localhost","content":{"body":"x","m.relates_to":`+relatesTo+`}}`)
	}

	reply := &relatedEvents{ctx: context.Background(), rsAPI: rs, event: event(`{"m.in_reply_to":{"event_id":"` + original.EventID() + `"}}`), enabled: true}
	got, fallback, err := reply.get("m.in_reply_to")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, original.EventID(), got.EventID())
	assert.False(t, fallback)
	if again, _, _ := reply.get("m.in_reply_to"); again == nil {
		t.Fatal("the cached event is lost")
	}
	got, _, _ = reply.get("m.thread")
	assert.Nil(t, got)
	assert.Equal(t, 1, rs.lookups, "the event is looked up once")

	// A reaction has no reply to look up for the reply rule.
	reactions := &relatedRoomserverAPI{events: rs.events}
	reaction := &relatedEvents{ctx: context.Background(), rsAPI: reactions, enabled: true, event: event(`{"rel_type":"m.annotation","event_id":"` + original.EventID() + `","key":"x"}`)}
	got, _, _ = reaction.get("m.in_reply_to")
	assert.Nil(t, got)
	assert.Zero(t, reactions.lookups)
	got, _, _ = reaction.get("m.annotation")
	require.NotNil(t, got)
	assert.Equal(t, 1, reactions.lookups)

	threadFallback := &relatedEvents{ctx: context.Background(), rsAPI: rs, enabled: true, event: event(fmt.Sprintf(
		`{"rel_type":"m.thread","event_id":%q,"is_falling_back":true,"m.in_reply_to":{"event_id":%q}}`, root.EventID(), original.EventID()))}
	got, fallback, _ = threadFallback.get("m.in_reply_to")
	require.NotNil(t, got)
	assert.True(t, fallback)
	got, fallback, _ = threadFallback.get("m.thread")
	require.NotNil(t, got)
	assert.Equal(t, root.EventID(), got.EventID())
	assert.False(t, fallback)

	threadReply := &relatedEvents{ctx: context.Background(), rsAPI: rs, enabled: true, event: event(fmt.Sprintf(
		`{"rel_type":"m.thread","event_id":%q,"is_falling_back":false,"m.in_reply_to":{"event_id":%q}}`, root.EventID(), original.EventID()))}
	_, fallback, _ = threadReply.get("m.in_reply_to")
	assert.False(t, fallback, "a real reply in a thread is not a fallback")

	otherRoom := &relatedRoomserverAPI{events: rs.events}
	crossRoom := &relatedEvents{ctx: context.Background(), rsAPI: otherRoom, enabled: true, event: event(`{"m.in_reply_to":{"event_id":"` + elsewhere.EventID() + `"}}`)}
	got, _, _ = crossRoom.get("m.in_reply_to")
	assert.Nil(t, got, "an event in another room is not related")

	off := &relatedRoomserverAPI{events: rs.events}
	disabled := &relatedEvents{ctx: context.Background(), rsAPI: off, event: reply.event}
	got, _, _ = disabled.get("m.in_reply_to")
	assert.Nil(t, got)
	assert.Zero(t, off.lookups, "nothing is looked up without the MSC")
	var none *relatedEvents
	got, _, err = none.get("m.in_reply_to")
	assert.NoError(t, err)
	assert.Nil(t, got)
}

// MSC3664: with the option, a reply to the user's event highlights through
// the reply rule, and a thread event whose reply is a fallback does not.
func TestEvaluatePushRulesReply(t *testing.T) {
	ctx := context.Background()
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		connStr, closeDB := test.PrepareDBConnectionString(t, dbType)
		defer closeDB()
		db, err := storage.NewUserDatabase(ctx, sqlutil.NewConnectionManager(nil, config.DatabaseOptions{}), &config.DatabaseOptions{
			ConnectionString: config.DataSource(connStr),
		}, "", 4, 0, 0, "", storage.WithPushRuleOptions(pushrules.Options{RelatedEventMatch: true}))
		require.NoError(t, err)

		original := mustCreateEvent(t, `{"type":"m.room.message","room_id":"!room:localhost","sender":"@test:localhost","content":{"body":"hi"}}`)
		root := mustCreateEvent(t, `{"type":"m.room.message","room_id":"!room:localhost","sender":"@other:localhost","content":{"body":"root"}}`)
		rs := &relatedRoomserverAPI{events: map[string]*types.HeaderedEvent{original.EventID(): original, root.EventID(): root}}
		consumer := OutputRoomEventConsumer{db: db, rsAPI: rs}
		mem := &localMembership{UserID: "@test:localhost", Localpart: "test", Domain: "localhost"}
		evaluate := func(relatesTo string) []*pushrules.Action {
			event := mustCreateEvent(t, `{"type":"m.room.message","room_id":"!room:localhost","sender":"@bob:localhost","content":{"body":"x","m.relates_to":`+relatesTo+`}}`)
			actions, evalErr := consumer.evaluatePushRules(ctx, event, mem, 10, eventThread{}, &relatedEvents{ctx: ctx, rsAPI: rs, event: event, enabled: true})
			require.NoError(t, evalErr)
			return actions
		}
		highlight := []*pushrules.Action{{Kind: pushrules.NotifyAction}, {Kind: pushrules.SetTweakAction, Tweak: pushrules.HighlightTweak}, {Kind: pushrules.SetTweakAction, Tweak: pushrules.SoundTweak, Value: "default"}}
		assert.Equal(t, highlight, evaluate(`{"m.in_reply_to":{"event_id":"`+original.EventID()+`"}}`))
		assert.Equal(t, []*pushrules.Action{{Kind: pushrules.NotifyAction}}, evaluate(`{"m.in_reply_to":{"event_id":"`+root.EventID()+`"}}`))
		assert.Equal(t, []*pushrules.Action{{Kind: pushrules.NotifyAction}}, evaluate(fmt.Sprintf(
			`{"rel_type":"m.thread","event_id":%q,"is_falling_back":true,"m.in_reply_to":{"event_id":%q}}`, root.EventID(), original.EventID())))
	})
}
