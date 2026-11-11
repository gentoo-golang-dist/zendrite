// Copyright 2024 New Vector Ltd.
// Copyright 2017 Jan Christian Grünhage
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package synctypes

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
)

// Filter is used by clients to specify how the server should filter responses to e.g. sync requests
// Specified by: https://spec.matrix.org/v1.6/client-server-api/#filtering
type Filter struct {
	EventFields []string    `json:"event_fields,omitempty"`
	EventFormat string      `json:"event_format,omitempty"`
	Presence    EventFilter `json:"presence,omitempty"`
	AccountData EventFilter `json:"account_data,omitempty"`
	Room        RoomFilter  `json:"room,omitempty"`
	// ProfileFields selects the profile fields whose updates /sync returns
	// (MSC4429). Without it, /sync returns none.
	ProfileFields *ProfileFieldsFilter `json:"org.matrix.msc4429.profile_fields,omitempty"`
}

// ProfileFieldsFilter lists the profile fields, by ID, whose updates /sync
// returns (MSC4429).
type ProfileFieldsFilter struct {
	IDs []string `json:"ids,omitempty"`
}

// ProfileFieldIDs returns the profile fields the filter asks /sync to return
// updates for, which is none when it does not opt in to MSC4429.
func (f *Filter) ProfileFieldIDs() []string {
	if f == nil || f.ProfileFields == nil {
		return nil
	}
	return f.ProfileFields.IDs
}

// EventFilter is used to define filtering rules for events.
type EventFilter struct {
	Limit      int       `json:"limit,omitempty"`
	NotSenders *[]string `json:"not_senders,omitempty"`
	NotTypes   *[]string `json:"not_types,omitempty"`
	Senders    *[]string `json:"senders,omitempty"`
	Types      *[]string `json:"types,omitempty"`
}

// RoomFilter is used to define filtering rules for room-related events.
type RoomFilter struct {
	NotRooms     *[]string       `json:"not_rooms,omitempty"`
	Rooms        *[]string       `json:"rooms,omitempty"`
	Ephemeral    RoomEventFilter `json:"ephemeral,omitempty"`
	IncludeLeave bool            `json:"include_leave,omitempty"`
	State        StateFilter     `json:"state,omitempty"`
	Timeline     RoomEventFilter `json:"timeline,omitempty"`
	AccountData  RoomEventFilter `json:"account_data,omitempty"`
}

// StateFilter is used to define filtering rules for state events.
type StateFilter struct {
	NotSenders                *[]string `json:"not_senders,omitempty"`
	NotTypes                  *[]string `json:"not_types,omitempty"`
	Senders                   *[]string `json:"senders,omitempty"`
	Types                     *[]string `json:"types,omitempty"`
	LazyLoadMembers           bool      `json:"lazy_load_members,omitempty"`
	IncludeRedundantMembers   bool      `json:"include_redundant_members,omitempty"`
	NotRooms                  *[]string `json:"not_rooms,omitempty"`
	Rooms                     *[]string `json:"rooms,omitempty"`
	Limit                     int       `json:"limit,omitempty"`
	UnreadThreadNotifications bool      `json:"unread_thread_notifications,omitempty"`
	ContainsURL               *bool     `json:"contains_url,omitempty"`
}

// RoomEventFilter is used to define filtering rules for events in rooms.
type RoomEventFilter struct {
	Limit                     int       `json:"limit,omitempty"`
	NotSenders                *[]string `json:"not_senders,omitempty"`
	NotTypes                  *[]string `json:"not_types,omitempty"`
	Senders                   *[]string `json:"senders,omitempty"`
	Types                     *[]string `json:"types,omitempty"`
	LazyLoadMembers           bool      `json:"lazy_load_members,omitempty"`
	IncludeRedundantMembers   bool      `json:"include_redundant_members,omitempty"`
	NotRooms                  *[]string `json:"not_rooms,omitempty"`
	Rooms                     *[]string `json:"rooms,omitempty"`
	UnreadThreadNotifications bool      `json:"unread_thread_notifications,omitempty"`
	ContainsURL               *bool     `json:"contains_url,omitempty"`
	// RelTypes keeps only the events with one of these relation types, and
	// NotRelTypes leaves out the events with one of them (MSC3874). Events
	// without a relation never match RelTypes and always pass NotRelTypes.
	RelTypes    *[]string `json:"org.matrix.msc3874.rel_types,omitempty"`
	NotRelTypes *[]string `json:"org.matrix.msc3874.not_rel_types,omitempty"`
	// Labels keeps only the events with one of these labels, and NotLabels
	// leaves out the events with one of them (MSC2326).
	Labels    *[]string `json:"org.matrix.labels,omitempty"`
	NotLabels *[]string `json:"org.matrix.not_labels,omitempty"`
}

// EventLabelsField is the event content field listing an event's labels
// (MSC2326), under the unstable prefix Synapse uses.
const EventLabelsField = "org.matrix.labels"

// MatchesRelationAndLabels reports whether an event whose relation type is
// relType ("" for none) and whose labels are labels passes the filter's
// MSC3874 relation type and MSC2326 label constraints. As in Synapse, an
// exclusion wins over an inclusion.
func (f *RoomEventFilter) MatchesRelationAndLabels(relType string, labels []string) bool {
	if f == nil {
		return true
	}
	if f.NotRelTypes != nil && relType != "" && slices.Contains(*f.NotRelTypes, relType) {
		return false
	}
	if f.RelTypes != nil && (relType == "" || !slices.Contains(*f.RelTypes, relType)) {
		return false
	}
	if f.NotLabels != nil && slices.ContainsFunc(labels, func(l string) bool { return slices.Contains(*f.NotLabels, l) }) {
		return false
	}
	if f.Labels != nil && !slices.ContainsFunc(labels, func(l string) bool { return slices.Contains(*f.Labels, l) }) {
		return false
	}
	return true
}

// FiltersRelationsOrLabels reports whether the filter constrains relation
// types (MSC3874) or labels (MSC2326).
func (f *RoomEventFilter) FiltersRelationsOrLabels() bool {
	return f != nil && (f.RelTypes != nil || f.NotRelTypes != nil || f.Labels != nil || f.NotLabels != nil)
}

// EventRelationType returns the relation type of an event's content, or ""
// when it has none. Like Synapse, a relation needs both a string rel_type
// and a string event_id.
func EventRelationType(content []byte) string {
	rel := gjson.GetBytes(content, `m\.relates_to`)
	relType, eventID := rel.Get("rel_type"), rel.Get("event_id")
	if relType.Type != gjson.String || eventID.Type != gjson.String || relType.Str == "" || eventID.Str == "" {
		return ""
	}
	return relType.Str
}

// EventLabels returns the labels of an event's content (MSC2326): the
// distinct strings of its org.matrix.labels array.
func EventLabels(content []byte) []string {
	field := gjson.GetBytes(content, `org\.matrix\.labels`)
	if !field.IsArray() {
		return nil
	}
	var labels []string
	for _, l := range field.Array() {
		if l.Type == gjson.String && !slices.Contains(labels, l.Str) {
			labels = append(labels, l.Str)
		}
	}
	return labels
}

const (
	EventFormatClient     = "client"
	EventFormatFederation = "federation"
)

// Validate checks if the filter contains valid property values.
func (filter *Filter) Validate() error {
	if filter.EventFormat != "" && filter.EventFormat != EventFormatClient && filter.EventFormat != EventFormatFederation {
		return errors.New("bad event_format value. Must be one of [\"client\", \"federation\"]")
	}
	for name, ids := range map[string]*[]string{
		"room.rooms":                  filter.Room.Rooms,
		"room.not_rooms":              filter.Room.NotRooms,
		"room.state.rooms":            filter.Room.State.Rooms,
		"room.state.not_rooms":        filter.Room.State.NotRooms,
		"room.timeline.rooms":         filter.Room.Timeline.Rooms,
		"room.timeline.not_rooms":     filter.Room.Timeline.NotRooms,
		"room.ephemeral.rooms":        filter.Room.Ephemeral.Rooms,
		"room.ephemeral.not_rooms":    filter.Room.Ephemeral.NotRooms,
		"room.account_data.rooms":     filter.Room.AccountData.Rooms,
		"room.account_data.not_rooms": filter.Room.AccountData.NotRooms,
	} {
		if err := validateIDs(name, ids, isRoomID); err != nil {
			return err
		}
	}
	for name, ids := range map[string]*[]string{
		"presence.senders":              filter.Presence.Senders,
		"presence.not_senders":          filter.Presence.NotSenders,
		"account_data.senders":          filter.AccountData.Senders,
		"account_data.not_senders":      filter.AccountData.NotSenders,
		"room.state.senders":            filter.Room.State.Senders,
		"room.state.not_senders":        filter.Room.State.NotSenders,
		"room.timeline.senders":         filter.Room.Timeline.Senders,
		"room.timeline.not_senders":     filter.Room.Timeline.NotSenders,
		"room.ephemeral.senders":        filter.Room.Ephemeral.Senders,
		"room.ephemeral.not_senders":    filter.Room.Ephemeral.NotSenders,
		"room.account_data.senders":     filter.Room.AccountData.Senders,
		"room.account_data.not_senders": filter.Room.AccountData.NotSenders,
	} {
		if err := validateIDs(name, ids, isUserID); err != nil {
			return err
		}
	}
	return nil
}

// validateIDs reports the first entry of ids that valid rejects.
// Like Synapse, it only checks the shape of each ID, so that a filter never
// fails because of an ID the server would accept elsewhere.
func validateIDs(name string, ids *[]string, valid func(string) bool) error {
	if ids == nil {
		return nil
	}
	for _, id := range *ids {
		if !valid(id) {
			return fmt.Errorf("%s contains an invalid ID %q", name, id)
		}
	}
	return nil
}

func isRoomID(id string) bool {
	return len(id) > 1 && id[0] == '!'
}

func isUserID(id string) bool {
	localpart, domain, found := strings.Cut(id, ":")
	return found && len(localpart) > 1 && localpart[0] == '@' && domain != ""
}

// DefaultFilter returns the default filter used by the Matrix server if no filter is provided in
// the request.
func DefaultFilter() Filter {
	return Filter{
		AccountData: DefaultEventFilter(),
		EventFields: nil,
		EventFormat: "client",
		Presence:    DefaultEventFilter(),
		Room: RoomFilter{
			AccountData:  DefaultRoomEventFilter(),
			Ephemeral:    DefaultRoomEventFilter(),
			IncludeLeave: false,
			NotRooms:     nil,
			Rooms:        nil,
			State:        DefaultStateFilter(),
			Timeline:     DefaultRoomEventFilter(),
		},
	}
}

// DefaultEventFilter returns the default event filter used by the Matrix server if no filter is
// provided in the request.
func DefaultEventFilter() EventFilter {
	return EventFilter{
		// parity with synapse: https://github.com/matrix-org/synapse/blob/v1.80.0/synapse/api/filtering.py#L336
		Limit:      10, //nolint:mnd
		NotSenders: nil,
		NotTypes:   nil,
		Senders:    nil,
		Types:      nil,
	}
}

// DefaultStateFilter returns the default state event filter used by the Matrix server if no filter
// is provided in the request.
func DefaultStateFilter() StateFilter {
	return StateFilter{
		NotSenders:              nil,
		NotTypes:                nil,
		Senders:                 nil,
		Types:                   nil,
		LazyLoadMembers:         false,
		IncludeRedundantMembers: false,
		NotRooms:                nil,
		Rooms:                   nil,
		ContainsURL:             nil,
	}
}

// DefaultRoomEventFilter returns the default room event filter used by the Matrix server if no
// filter is provided in the request.
func DefaultRoomEventFilter() RoomEventFilter {
	return RoomEventFilter{
		// parity with synapse: https://github.com/matrix-org/synapse/blob/v1.80.0/synapse/api/filtering.py#L336
		Limit:       10, //nolint:mnd
		NotSenders:  nil,
		NotTypes:    nil,
		Senders:     nil,
		Types:       nil,
		NotRooms:    nil,
		Rooms:       nil,
		ContainsURL: nil,
	}
}
