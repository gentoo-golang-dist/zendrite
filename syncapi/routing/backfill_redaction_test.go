// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/test"
)

type redactingBackfillRoomserver struct {
	api.SyncRoomserverAPI
	userID     spec.UserID
	events     []*rstypes.HeaderedEvent
	redactions []api.OutputRedactedEvent
}

func (s *redactingBackfillRoomserver) PerformBackfill(_ context.Context, _ *api.PerformBackfillRequest, res *api.PerformBackfillResponse) error {
	res.Events = s.events
	res.Redactions = s.redactions
	return nil
}

func (s *redactingBackfillRoomserver) QueryUserIDForSender(context.Context, spec.RoomID, spec.SenderID) (*spec.UserID, error) {
	return &s.userID, nil
}

// A backfilled redaction can target an event the sync API already holds,
// which the backfill does not return, such as one received live before the
// redaction was missed.
func TestBackfillRedactsEventAlreadyStored(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		connStr, closeDB := test.PrepareDBConnectionString(t, dbType)
		defer closeDB()
		options := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
		cm := sqlutil.NewConnectionManager(nil, config.DatabaseOptions{})
		db, err := storage.NewSyncServerDatasource(t.Context(), cm, &options)
		require.NoError(t, err)

		alice := test.NewUser(t)
		room := test.NewRoom(t, alice)
		message := room.CreateAndInsert(t, alice, "m.room.message", map[string]string{"body": "secret"})
		redaction := room.CreateAndInsert(t, alice, spec.MRoomRedaction,
			map[string]string{"redacts": message.EventID()}, test.WithRedacts(message.EventID()))
		_, err = db.WriteEvent(t.Context(), message, nil, nil, nil, nil, false, gomatrixserverlib.HistoryVisibilityShared)
		require.NoError(t, err)

		userID, err := spec.NewUserID(alice.ID, true)
		require.NoError(t, err)
		rs := &redactingBackfillRoomserver{
			userID: *userID,
			events: []*rstypes.HeaderedEvent{redaction},
			redactions: []api.OutputRedactedEvent{
				{RedactedEventID: message.EventID(), RedactedBecause: redaction},
			},
		}
		_, err = backfillEvents(t.Context(), rs, db, "local", "local", room.ID, nil, 10)
		require.NoError(t, err)

		stored, err := db.Events(t.Context(), []string{message.EventID()})
		require.NoError(t, err)
		require.Len(t, stored, 1)
		require.False(t, gjson.GetBytes(stored[0].Content(), "body").Exists())
		require.Equal(t, redaction.EventID(), gjson.GetBytes(stored[0].Unsigned(), "redacted_because.event_id").Str)
	})
}
