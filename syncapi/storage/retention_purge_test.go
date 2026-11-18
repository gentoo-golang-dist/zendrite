// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package storage_test

import (
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	rstypes "codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/syncapi/storage/shared"
	"codefloe.com/pat-s/zendrite/test"
)

// TestPurgeExpiredEventsKeepsStateDeltas checks that the MSC1763 purge
// deletes an expired message, but only redacts one whose row records state
// changes that incremental syncs need.
func TestPurgeExpiredEventsKeepsStateDeltas(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		room := test.NewRoom(t, alice)
		db, closeDB := MustCreateDatabase(t, dbType)
		defer closeDB()
		MustWriteEvents(t, db, room.Events())

		old := time.Now().Add(-2 * time.Hour)
		plain := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "plain"}, test.WithTimestamp(old))
		topic := room.CreateAndInsert(t, alice, spec.MRoomTopic, map[string]any{"topic": "t"}, test.WithStateKey(""), test.WithTimestamp(old))
		withDelta := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "with delta"}, test.WithTimestamp(old))
		latest := room.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "latest"})
		MustWriteEvents(t, db, []*rstypes.HeaderedEvent{plain, topic})
		_, err := db.WriteEvent(ctx, withDelta, nil, []string{topic.EventID()}, nil, nil, false, gomatrixserverlib.HistoryVisibilityShared)
		require.NoError(t, err)
		MustWriteEvents(t, db, []*rstypes.HeaderedEvent{latest})

		cursor := shared.RetentionCursor{TS: time.Now().Add(-time.Hour).UnixMilli() - 1}
		purged, _, done, err := db.PurgeExpiredEvents(ctx, room.ID, cursor, 100)
		require.NoError(t, err)
		assert.True(t, done)
		assert.ElementsMatch(t, []string{plain.EventID(), withDelta.EventID()}, purged)

		// A second pass leaves the redacted row alone.
		purged, _, _, err = db.PurgeExpiredEvents(ctx, room.ID, cursor, 100)
		require.NoError(t, err)
		assert.Empty(t, purged)

		events, err := db.Events(ctx, []string{plain.EventID(), topic.EventID(), withDelta.EventID(), latest.EventID()})
		require.NoError(t, err)
		byID := map[string]gomatrixserverlib.PDU{}
		for _, ev := range events {
			byID[ev.EventID()] = ev.PDU
		}
		assert.NotContains(t, byID, plain.EventID())
		assert.Contains(t, byID, topic.EventID())
		assert.Contains(t, byID, latest.EventID())
		require.Contains(t, byID, withDelta.EventID())
		assert.False(t, gjson.GetBytes(byID[withDelta.EventID()].Content(), "body").Exists(), "the kept row is redacted")
	})
}
