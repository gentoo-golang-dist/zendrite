package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/storage/shared"
	"codefloe.com/pat-s/zendrite/syncapi/storage/tables"
	"codefloe.com/pat-s/zendrite/syncapi/streams"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

type threadedReceiptDatabase struct {
	storage.Database
	snapshot *threadedReceiptSnapshot
	// joined are the rooms the user is joined to.
	joined []string
}

func (d threadedReceiptDatabase) NewDatabaseSnapshot(context.Context) (*shared.DatabaseTransaction, error) {
	return &shared.DatabaseTransaction{Database: &shared.Database{Receipts: d.snapshot, CurrentRoomState: joinedSnapshot{rooms: d.joined}}}, nil
}

type threadedReceiptSnapshot struct {
	tables.Receipts
	pending []types.OutputReceiptEvent
}

func (s *threadedReceiptSnapshot) SelectLatestUserReceiptsForConnection(_ context.Context, _ *sql.Tx, _ int64, roomIDs []string, _ string) ([]types.OutputReceiptEvent, error) {
	var receipts []types.OutputReceiptEvent
	for _, receipt := range s.pending {
		if slices.Contains(roomIDs, receipt.RoomID) {
			receipts = append(receipts, receipt)
		}
	}
	return receipts, nil
}

// joinedSnapshot is a snapshot in which the user is joined to rooms.
type joinedSnapshot struct {
	tables.CurrentRoomState
	rooms []string
}

func (s joinedSnapshot) SelectRoomIDsWithMembership(context.Context, *sql.Tx, string, string) ([]string, error) {
	return s.rooms, nil
}

func joinedTo(rooms ...string) *shared.DatabaseTransaction {
	return &shared.DatabaseTransaction{Database: &shared.Database{CurrentRoomState: joinedSnapshot{rooms: rooms}}}
}

func TestSlidingReceiptThreadCollision(t *testing.T) {
	snapshot := &threadedReceiptSnapshot{pending: []types.OutputReceiptEvent{
		{RoomID: "!room:test", EventID: "$same", UserID: "@alice:test", Type: "m.read", Timestamp: 1},
		{RoomID: "!room:test", EventID: "$same", UserID: "@alice:test", Type: "m.read", ThreadID: "main", Timestamp: 2},
		{RoomID: "!room:test", EventID: "$reply", UserID: "@bob:test", Type: "m.read.private", ThreadID: "$root", Timestamp: 3},
	}}
	pool := RequestPool{db: threadedReceiptDatabase{snapshot: snapshot, joined: []string{"!room:test"}}}
	for _, wantThread := range []string{"", "main"} {
		response, _, delivered, err := pool.processReceiptsExtension(t.Context(), nil, 1, "@alice:test", nil, []string{"!room:test"}, nil, types.StreamingToken{}, nil, map[string]bool{"!room:test": true}, nil)
		require.NoError(t, err)
		require.Len(t, delivered, 1)
		require.Equal(t, wantThread, delivered[0].ThreadID)
		var content map[string]map[string]map[string]ReceiptTS
		require.NoError(t, json.Unmarshal(response.Rooms["!room:test"].Content, &content))
		require.Equal(t, wantThread, content["$same"]["m.read"]["@alice:test"].ThreadID)
		require.NotContains(t, content, "$reply")
		for i, receipt := range snapshot.pending {
			if receipt == delivered[0] {
				snapshot.pending = append(snapshot.pending[:i], snapshot.pending[i+1:]...)
				break
			}
		}
	}
}

func TestSlidingReceiptsLimitedOnRoomReset(t *testing.T) {
	snapshot := &threadedReceiptSnapshot{pending: []types.OutputReceiptEvent{
		{RoomID: "!reset:test", EventID: "$old", UserID: "@alice:test", Type: "m.read", Timestamp: 1},
		{RoomID: "!reset:test", EventID: "$old", UserID: "@alice:test", Type: "m.read.private", ThreadID: "$root", Timestamp: 2},
		{RoomID: "!reset:test", EventID: "$recent", UserID: "@bob:test", Type: "m.read", Timestamp: 3},
		{RoomID: "!reset:test", EventID: "$old", UserID: "@carol:test", Type: "m.read", Timestamp: 4},
		{RoomID: "!expanded:test", EventID: "$older", UserID: "@dave:test", Type: "m.read", Timestamp: 5},
		{RoomID: "!live:test", EventID: "$old", UserID: "@erin:test", Type: "m.read", Timestamp: 6},
	}}
	rooms := []string{"!reset:test", "!expanded:test", "!live:test"}
	pool := RequestPool{db: threadedReceiptDatabase{snapshot: snapshot, joined: rooms}}
	subscriptions := map[string]bool{"!reset:test": true, "!expanded:test": true, "!live:test": true}
	responseRooms := map[string]types.SlidingRoomData{
		"!reset:test":    {Initial: true, Timeline: []synctypes.ClientEvent{{EventID: "$recent"}}},
		"!expanded:test": {ExpandedTimeline: true, Timeline: []synctypes.ClientEvent{{EventID: "$newer"}}},
		"!live:test":     {Timeline: []synctypes.ClientEvent{{EventID: "$newest"}}},
	}

	response, _, delivered, err := pool.processReceiptsExtension(t.Context(), nil, 1, "@alice:test", nil, rooms, nil, types.StreamingToken{}, nil, subscriptions, responseRooms)
	require.NoError(t, err)

	sent := func(roomID string) map[string]map[string]map[string]ReceiptTS {
		ev, ok := response.Rooms[roomID]
		if !ok {
			return nil
		}
		var content map[string]map[string]map[string]ReceiptTS
		require.NoError(t, json.Unmarshal(ev.Content, &content))
		return content
	}

	reset := sent("!reset:test")
	require.Contains(t, reset["$old"]["m.read"], "@alice:test", "own receipts are sent even outside the window")
	require.Contains(t, reset["$old"]["m.read.private"], "@alice:test", "own private and threaded receipts are sent")
	require.Contains(t, reset["$recent"]["m.read"], "@bob:test", "receipts in the window are sent")
	require.NotContains(t, reset["$old"]["m.read"], "@carol:test", "other users' receipts outside the window are withheld")
	require.Nil(t, sent("!expanded:test"), "an expanded room with only out-of-window receipts gets no receipt event")
	require.Contains(t, sent("!live:test")["$old"]["m.read"], "@erin:test", "rooms that were not reset are unaffected")

	// Withheld receipts are recorded as delivered so the next response does not
	// carry them, which would move the whole payload one request later.
	require.ElementsMatch(t, snapshot.pending, delivered)
}

// TestSlidingReceiptsJoinedRoomsOnly checks that rooms in scope the user is
// not joined to, such as invites and rooms the user was kicked from, which
// lists keep, get no receipts.
func TestSlidingReceiptsJoinedRoomsOnly(t *testing.T) {
	snapshot := &threadedReceiptSnapshot{pending: []types.OutputReceiptEvent{
		{RoomID: "!joined:test", EventID: "$a", UserID: "@bob:test", Type: "m.read", Timestamp: 1},
		{RoomID: "!invited:test", EventID: "$b", UserID: "@bob:test", Type: "m.read", Timestamp: 2},
	}}
	pool := RequestPool{db: threadedReceiptDatabase{snapshot: snapshot, joined: []string{"!joined:test", "!other:test"}}}
	subscriptions := map[string]bool{"!joined:test": true, "!invited:test": true}

	response, _, delivered, err := pool.processReceiptsExtension(t.Context(), nil, 1, "@alice:test", nil, nil, nil, types.StreamingToken{}, nil, subscriptions, nil)
	require.NoError(t, err)
	require.Contains(t, response.Rooms, "!joined:test")
	require.NotContains(t, response.Rooms, "!invited:test")
	// The invited room's receipt is not recorded as delivered either, so it is
	// sent once the user joins.
	require.Len(t, delivered, 1)
	require.Equal(t, "!joined:test", delivered[0].RoomID)
}

// TestSlidingTypingJoinedRoomsOnly checks that rooms in scope the user is not
// joined to get no typing notifications.
func TestSlidingTypingJoinedRoomsOnly(t *testing.T) {
	cache := caching.NewTypingCache(t.Context())
	for _, roomID := range []string{"!joined:test", "!invited:test"} {
		cache.AddTypingUser("@bob:test", roomID, nil)
	}
	pool := RequestPool{streams: &streams.Streams{TypingStreamProvider: &streams.TypingStreamProvider{EDUCache: cache}}}
	subscriptions := map[string]bool{"!joined:test": true, "!invited:test": true}

	response, err := pool.processTypingExtension(t.Context(), joinedTo("!joined:test"), "@alice:test", nil, nil, nil, types.StreamingToken{}, nil, subscriptions, nil)
	require.NoError(t, err)
	require.Contains(t, response.Rooms, "!joined:test")
	require.NotContains(t, response.Rooms, "!invited:test")
}
