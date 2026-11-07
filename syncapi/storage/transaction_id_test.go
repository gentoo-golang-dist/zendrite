package storage_test

import (
	"context"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/test"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

type senderIDRoomserver struct {
	api.SyncRoomserverAPI
}

func (senderIDRoomserver) QuerySenderIDForUser(_ context.Context, _ spec.RoomID, userID spec.UserID) (*spec.SenderID, error) {
	sender := spec.SenderID(userID.String())
	return &sender, nil
}

// An event fetched by ID carries the transaction ID only for the device that
// sent it.
func TestEventsForDeviceAddsTransactionID(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		alice := test.NewUser(t)
		r := test.NewRoom(t, alice)
		db, close := MustCreateDatabase(t, dbType)
		defer close()
		MustWriteEvents(t, db, r.Events())

		ev := r.CreateAndInsert(t, alice, "m.room.message", map[string]any{"body": "hi"})
		txnID := &api.TransactionID{SessionID: 7, TransactionID: "txn1"}
		if _, err := db.WriteEvent(ctx, ev, nil, nil, nil, txnID, false, gomatrixserverlib.HistoryVisibilityShared); err != nil {
			t.Fatal(err)
		}

		for name, tc := range map[string]struct {
			device *userapi.Device
			want   string
		}{
			"sending device": {&userapi.Device{UserID: alice.ID, SessionID: 7}, "txn1"},
			"other device":   {&userapi.Device{UserID: alice.ID, SessionID: 8}, ""},
			"other user":     {&userapi.Device{UserID: "@bob:test", SessionID: 7}, ""},
		} {
			WithSnapshot(t, db, func(snapshot storage.DatabaseTransaction) {
				events, err := snapshot.EventsForDevice(ctx, tc.device, []string{ev.EventID()}, senderIDRoomserver{})
				if err != nil {
					t.Fatal(err)
				}
				if len(events) != 1 {
					t.Fatalf("%s: got %d events, want 1", name, len(events))
				}
				if got := gjson.GetBytes(events[0].JSON(), "unsigned.transaction_id").Str; got != tc.want {
					t.Errorf("%s: transaction_id = %q, want %q", name, got, tc.want)
				}
			})
		}
	})
}
