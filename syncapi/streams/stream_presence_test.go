package streams

import (
	"context"
	"testing"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/syncapi/notifier"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/test"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

type presenceSnapshot struct {
	storage.DatabaseTransaction
	presence types.PresenceInternal
}

func (s presenceSnapshot) PresenceAfter(context.Context, types.StreamPosition, synctypes.EventFilter) (map[string]*types.PresenceInternal, error) {
	presence := s.presence
	return map[string]*types.PresenceInternal{presence.UserID: &presence}, nil
}

func (s presenceSnapshot) GetPresences(context.Context, []string) ([]*types.PresenceInternal, error) {
	return nil, nil
}

// senderIsUserRoomserver resolves sender IDs as user IDs, as in rooms without pseudo IDs.
type senderIsUserRoomserver struct {
	api.SyncRoomserverAPI
}

func (senderIsUserRoomserver) QueryUserIDForSender(_ context.Context, _ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
	return spec.NewUserID(string(senderID), true)
}

func TestPresenceCompleteSyncIgnoresSentPresence(t *testing.T) {
	alice, bob := test.NewUser(t), test.NewUser(t)
	room := test.NewRoom(t, alice)
	room.CreateAndInsert(t, bob, spec.MRoomMember, map[string]any{"membership": spec.Join}, test.WithStateKey(bob.ID))

	// Let the notifier know that Alice and Bob share the room.
	n := notifier.NewNotifier(senderIsUserRoomserver{})
	for _, ev := range room.Events() {
		n.OnNewEvent(ev, "", nil, types.StreamingToken{})
	}
	provider := &PresenceStreamProvider{notifier: n}

	snapshot := presenceSnapshot{presence: types.PresenceInternal{
		ClientFields: types.PresenceClientResponse{Presence: "online"},
		StreamPos:    5,
		UserID:       bob.ID,
		LastActiveTS: spec.AsTimestamp(time.Now()),
	}}

	newRequest := func() *types.SyncRequest {
		req := &types.SyncRequest{
			Device:   &userapi.Device{UserID: alice.ID, ID: "DEVICE"},
			Response: types.NewResponse(),
			Log:      logrus.NewEntry(logrus.New()),
		}
		req.Filter.Presence.Limit = 10
		return req
	}

	// Each complete sync starts without any state, so it must include Bob's presence,
	// even when the device was sent the same presence before.
	for range 2 {
		req := newRequest()
		provider.CompleteSync(t.Context(), snapshot, req)
		require.Len(t, req.Response.Presence.Events, 1)
		require.Equal(t, bob.ID, req.Response.Presence.Events[0].Sender)
	}

	// An incremental sync still skips presence the device already has.
	req := newRequest()
	provider.IncrementalSync(t.Context(), snapshot, req, 0, 5)
	require.Empty(t, req.Response.Presence.Events)
}
