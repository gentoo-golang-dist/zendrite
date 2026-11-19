// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only.

package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	rsapi "codefloe.com/pat-s/zendrite/roomserver/api"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/userapi/api"
	"codefloe.com/pat-s/zendrite/userapi/storage"
	"codefloe.com/pat-s/zendrite/userapi/storage/tables"
)

func TestQueryNotifications(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := t.Context()
		conn, closeDB := test.PrepareDBConnectionString(t, dbType)
		defer closeDB()
		db, err := storage.NewUserDatabase(ctx, sqlutil.NewConnectionManager(nil, config.DatabaseOptions{}), &config.DatabaseOptions{ConnectionString: config.DataSource(conn)}, "localhost", 4, 0, api.DefaultLoginTokenLifetime, "_server")
		require.NoError(t, err)
		userAPI := &UserInternalAPI{DB: db}
		for i := 1; i <= 5; i++ {
			eventID := fmt.Sprintf("$event%d", i)
			n := &api.Notification{RoomID: "!room:localhost", TS: spec.Timestamp(i), Event: synctypes.ClientEvent{EventID: eventID, Content: spec.RawJSON(`{}`)}}
			require.NoError(t, db.InsertNotification(ctx, "alice", "localhost", eventID, uint64(i), map[string]any{"highlight": i%2 == 0}, n))
		}
		// Other users and virtual hosts must never appear in Alice's results.
		for _, owner := range []struct {
			localpart string
			server    spec.ServerName
		}{{"bob", "localhost"}, {"alice", "elsewhere"}} {
			require.NoError(t, db.InsertNotification(ctx, owner.localpart, owner.server, "$other", 6, nil, &api.Notification{RoomID: "!room:localhost", Event: synctypes.ClientEvent{Content: spec.RawJSON(`{}`)}}))
		}
		_, err = db.SetNotificationsRead(ctx, "alice", "localhost", "!room:localhost", 2, true)
		require.NoError(t, err)
		for _, tc := range []struct {
			name, only string
			ids        []string
		}{
			{"all", "", []string{"$event1", "$event2", "$event3", "$event4", "$event5"}},
			{"highlights", "highlight", []string{"$event2", "$event4"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var res api.QueryNotificationsResponse
				var ids []string
				for page := 0; ; page++ {
					require.Less(t, page, 10, "pagination must terminate")
					require.NoError(t, userAPI.QueryNotifications(ctx, &api.QueryNotificationsRequest{Localpart: "alice", ServerName: "localhost", From: res.NextToken, Limit: 1, Only: tc.only}, &res))
					for _, n := range res.Notifications {
						ids = append(ids, n.Event.EventID)
						require.Equal(t, n.TS <= 2, n.Read)
					}
					if res.NextToken == "" {
						break
					}
				}
				require.Equal(t, tc.ids, ids)
				encoded, err := json.Marshal(res)
				require.NoError(t, err)
				require.JSONEq(t, `{"notifications":[]}`, string(encoded))
			})
		}
		count, err := db.GetNotificationCount(ctx, "alice", "localhost", tables.AllNotifications)
		require.NoError(t, err)
		require.EqualValues(t, 3, count, "badge counts still exclude read notifications")
	})
}

// retentionRoomserver serves the m.room.retention events of rooms.
type retentionRoomserver struct {
	rsapi.UserRoomserverAPI
	policies map[string]*rstypes.HeaderedEvent
}

func (r *retentionRoomserver) QueryLatestEventsAndState(_ context.Context, req *rsapi.QueryLatestEventsAndStateRequest, res *rsapi.QueryLatestEventsAndStateResponse) error {
	res.RoomExists = true
	if ev, ok := r.policies[req.RoomID]; ok {
		res.StateEvents = []*rstypes.HeaderedEvent{ev}
	}
	return nil
}

// TestQueryNotificationsRetention checks MSC1763: notifications of events
// past their room's retention policy are left out, and pages are filled
// from later notifications rather than coming back short.
func TestQueryNotificationsRetention(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := t.Context()
		conn, closeDB := test.PrepareDBConnectionString(t, dbType)
		defer closeDB()
		db, err := storage.NewUserDatabase(ctx, sqlutil.NewConnectionManager(nil, config.DatabaseOptions{}), &config.DatabaseOptions{ConnectionString: config.DataSource(conn)}, "localhost", 4, 0, api.DefaultLoginTokenLifetime, "_server")
		require.NoError(t, err)

		alice := test.NewUser(t)
		hourRoom := test.NewRoom(t, alice)
		openRoom := test.NewRoom(t, alice)
		global := &config.Global{Retention: config.Retention{Enabled: true}}
		userAPI := &UserInternalAPI{
			DB:     db,
			Config: &config.UserAPI{Matrix: global},
			RSAPI: &retentionRoomserver{policies: map[string]*rstypes.HeaderedEvent{
				hourRoom.ID: hourRoom.CreateEvent(t, alice, "m.room.retention", map[string]any{"max_lifetime": time.Hour.Milliseconds()}, test.WithStateKey("")),
			}},
		}

		now := time.Now()
		stateKey := ""
		insert := func(id int, roomID string, age time.Duration, state bool) string {
			eventID := fmt.Sprintf("$event%d", id)
			ev := synctypes.ClientEvent{EventID: eventID, Content: spec.RawJSON(`{}`), OriginServerTS: spec.AsTimestamp(now.Add(-age))}
			if state {
				ev.StateKey = &stateKey
			}
			n := &api.Notification{RoomID: roomID, TS: spec.Timestamp(id), Event: ev}
			require.NoError(t, db.InsertNotification(ctx, "alice", "localhost", eventID, uint64(id), nil, n))
			return eventID
		}
		var want []string
		// Three expired notifications in a row fill no page.
		for i := 1; i <= 3; i++ {
			insert(i, hourRoom.ID, 2*time.Hour, false)
		}
		want = append(want,
			insert(4, hourRoom.ID, 10*time.Minute, false),
			// State events never expire.
			insert(5, hourRoom.ID, 2*time.Hour, true),
			// The other room has no policy.
			insert(6, openRoom.ID, 2*time.Hour, false),
		)
		insert(7, hourRoom.ID, 2*time.Hour, false)
		want = append(want, insert(8, openRoom.ID, time.Minute, false))
		insert(9, hourRoom.ID, 2*time.Hour, false)

		for _, limit := range []int{1, 2, 10} {
			var res api.QueryNotificationsResponse
			var got []string
			for page := 0; ; page++ {
				require.Less(t, page, 20, "pagination must terminate")
				require.NoError(t, userAPI.QueryNotifications(ctx, &api.QueryNotificationsRequest{Localpart: "alice", ServerName: "localhost", From: res.NextToken, Limit: limit}, &res))
				for _, n := range res.Notifications {
					got = append(got, n.Event.EventID)
				}
				if len(got) < len(want) {
					require.Len(t, res.Notifications, limit, "a page before the end is full")
				}
				if res.NextToken == "" {
					break
				}
			}
			require.Equal(t, want, got, "limit %d", limit)
		}

		// With retention disabled, every notification is listed.
		global.Retention.Enabled = false
		var res api.QueryNotificationsResponse
		require.NoError(t, userAPI.QueryNotifications(ctx, &api.QueryNotificationsRequest{Localpart: "alice", ServerName: "localhost", Limit: 20}, &res))
		require.Len(t, res.Notifications, 9)
	})
}
