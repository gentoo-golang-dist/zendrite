package pushrules

import "encoding/json"

// A Condition dictates extra conditions for a matching rules. See
// ConditionKind.
type Condition struct {
	// Kind is the primary discriminator for the condition
	// type. Required.
	Kind ConditionKind `json:"kind"`

	// Key indicates the dot-separated path of Event fields to
	// match. Required for EventMatchCondition and
	// SenderNotificationPermissionCondition.
	Key string `json:"key,omitempty"`

	// Pattern indicates the value pattern that must match. Required
	// for EventMatchCondition.
	Pattern *string `json:"pattern,omitempty"`

	// Value is a non-compound JSON value for property conditions.
	// RawMessage distinguishes a missing value from an explicit null.
	Value json.RawMessage `json:"value,omitempty"`

	// Is indicates the condition that must be fulfilled. Required for
	// RoomMemberCountCondition.
	Is string `json:"is,omitempty"`

	// Subscribed is whether the user must be subscribed to the event's
	// thread. Required for ThreadSubscriptionCondition.
	Subscribed *bool `json:"subscribed,omitempty"`
}

// ConditionKind represents a kind of condition.
//
// SPEC: Unrecognized conditions MUST NOT match any events,
// effectively making the push rule disabled.
type ConditionKind string

const (
	UnknownCondition ConditionKind = ""

	EventPropertyIsCondition       ConditionKind = "event_property_is"
	EventPropertyContainsCondition ConditionKind = "event_property_contains"

	// EventMatchCondition indicates the condition looks for a key
	// path and matches a pattern. How paths that don't reference a
	// simple value match against rules is implementation-specific.
	EventMatchCondition ConditionKind = "event_match"

	// ContainsDisplayNameCondition indicates the current user's
	// display name must be found in the content body. MSC4210 (spec v1.17)
	// deprecated it; it is still evaluated for user-defined rules.
	ContainsDisplayNameCondition ConditionKind = "contains_display_name"

	// RoomMemberCountCondition matches a simple arithmetic comparison
	// against the total number of members in a room.
	RoomMemberCountCondition ConditionKind = "room_member_count"

	// SenderNotificationPermissionCondition compares power level for
	// the sender in the event's room.
	SenderNotificationPermissionCondition ConditionKind = "sender_notification_permission"

	// ThreadSubscriptionCondition matches events in a thread that the user
	// is (or, with subscribed false, is not) subscribed to (MSC4306,
	// unstable name).
	ThreadSubscriptionCondition ConditionKind = "io.element.msc4306.thread_subscription"
)
