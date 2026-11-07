package synctypes

import (
	"encoding/json"
	"reflect"
	"testing"
)

func Test_Filter(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  RoomEventFilter
	}{
		{
			name:  "empty types filter",
			input: []byte(`{ "types": [] }`),
			want: RoomEventFilter{
				Limit:                   0,
				NotSenders:              nil,
				NotTypes:                nil,
				Senders:                 nil,
				Types:                   &[]string{},
				LazyLoadMembers:         false,
				IncludeRedundantMembers: false,
				NotRooms:                nil,
				Rooms:                   nil,
				ContainsURL:             nil,
			},
		},
		{
			name:  "absent types filter",
			input: []byte(`{}`),
			want: RoomEventFilter{
				Limit:                   0,
				NotSenders:              nil,
				NotTypes:                nil,
				Senders:                 nil,
				Types:                   nil,
				LazyLoadMembers:         false,
				IncludeRedundantMembers: false,
				NotRooms:                nil,
				Rooms:                   nil,
				ContainsURL:             nil,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f RoomEventFilter
			if err := json.Unmarshal(tt.input, &f); err != nil {
				t.Fatalf("unable to parse filter: %v", err)
			}
			if !reflect.DeepEqual(f, tt.want) {
				t.Fatalf("Expected %+v\ngot %+v", tt.want, f)
			}
		})
	}
}

func TestFilterValidate(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "empty filter", input: `{}`},
		{name: "valid room IDs", input: `{"room":{"rooms":["!a:b"],"timeline":{"not_rooms":["!abc"]}}}`},
		{name: "valid senders", input: `{"presence":{"senders":["@a:b"]},"room":{"timeline":{"not_senders":["@a:b:8448"]}}}`},
		{name: "invalid event format", input: `{"event_format":"nope"}`, wantErr: true},
		{name: "invalid timeline room", input: `{"room":{"timeline":{"rooms":["not_a_room_id"]}}}`, wantErr: true},
		{name: "invalid top-level not_rooms", input: `{"room":{"not_rooms":["!"]}}`, wantErr: true},
		{name: "invalid timeline sender", input: `{"room":{"timeline":{"senders":["not_a_sender_id"]}}}`, wantErr: true},
		{name: "sender without domain", input: `{"room":{"state":{"not_senders":["@alice"]}}}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f Filter
			if err := json.Unmarshal([]byte(tt.input), &f); err != nil {
				t.Fatalf("unable to parse filter: %v", err)
			}
			if err := f.Validate(); (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
