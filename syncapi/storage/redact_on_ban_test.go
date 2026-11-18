// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package storage_test

import (
	"context"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/test"
)

// MSC4293: a ban that redacts the target's events is their redaction, and
// is stored as their redacted_because.
func TestRedactEventBecauseOfBan(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice, bob := test.NewUser(t), test.NewUser(t)
		room := test.NewRoom(t, alice, test.RoomVersion("10"))
		room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))
		spam := room.CreateAndInsert(t, bob, "m.room.message", map[string]any{"body": "spam"})
		ban := room.CreateAndInsert(t, alice, spec.MRoomMember, map[string]any{
			"membership": spec.Ban, "org.matrix.msc4293.redact_events": true,
		}, test.WithStateKey(bob.ID))

		db, closeDB := MustCreateDatabase(t, dbType)
		defer closeDB()
		MustWriteEvents(t, db, room.Events())
		require.NoError(t, db.RedactEvent(context.Background(), spam.EventID(), ban, &FakeQuerier{}))

		snapshot, err := db.NewDatabaseSnapshot(context.Background())
		require.NoError(t, err)
		defer snapshot.Rollback() //nolint:errcheck
		events, err := snapshot.Events(context.Background(), []string{spam.EventID()})
		require.NoError(t, err)
		require.Len(t, events, 1)
		require.False(t, gjson.GetBytes(events[0].Content(), "body").Exists())
		require.Equal(t, spec.Ban, gjson.GetBytes(events[0].Unsigned(), "redacted_because.content.membership").Str)
		require.Equal(t, ban.EventID(), gjson.GetBytes(events[0].Unsigned(), "redacted_by").Str)
	})
}
