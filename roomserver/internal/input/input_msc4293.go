// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// redactEventsPath is api.RedactEventsKey as a gjson path.
var redactEventsPath = strings.ReplaceAll(api.RedactEventsKey, ".", `\.`)

// redactionBatch bounds the events redacted in one transaction.
const redactionBatch = 100

// kickOrBanRedacts reports whether a membership event is a kick or ban that
// asks for the target's events to be redacted (MSC4293), and returns the
// target's membership before it. A leave counts as a kick if another user
// sent it and the target was joined, invited or knocking; the flag has no
// effect on any other membership, such as a voluntary leave or an unban.
func (r *Inputer) kickOrBanRedacts(ctx context.Context, roomInfo *types.RoomInfo, event gomatrixserverlib.PDU) (bool, *types.Event, error) {
	if event.Type() != spec.MRoomMember || event.StateKey() == nil || *event.StateKey() == string(event.SenderID()) ||
		gjson.GetBytes(event.Content(), redactEventsPath).Type != gjson.True {
		return false, nil, nil
	}
	membership, err := event.Membership()
	if err != nil || (membership != spec.Ban && membership != spec.Leave) {
		return false, nil, nil
	}
	previous, err := r.previousMembership(ctx, roomInfo, event)
	if err != nil {
		return false, nil, err
	}
	if membership == spec.Leave {
		if previous == nil {
			return false, nil, nil
		}
		switch before, _ := previous.Membership(); before {
		case spec.Join, spec.Invite, spec.Knock:
		default:
			return false, nil, nil
		}
	}
	return true, previous, nil
}

// previousMembership returns the target's membership event before a
// membership event, which is among its auth events, or nil if there is none.
func (r *Inputer) previousMembership(ctx context.Context, roomInfo *types.RoomInfo, event gomatrixserverlib.PDU) (*types.Event, error) {
	authEvents, err := r.DB.EventsFromIDs(ctx, roomInfo, event.AuthEventIDs())
	if err != nil {
		return nil, fmt.Errorf("r.DB.EventsFromIDs: %w", err)
	}
	for i := range authEvents {
		if authEvents[i].Type() == spec.MRoomMember && event.StateKey() != nil && authEvents[i].StateKeyEquals(*event.StateKey()) {
			return &authEvents[i], nil
		}
	}
	return nil, nil
}

// sessionStart returns the NID of the membership event that began the
// target's current stay in the room: the join before a kick or ban,
// skipping the joins that only changed the target's profile. The events
// sent after it are redacted.
func (r *Inputer) sessionStart(ctx context.Context, roomInfo *types.RoomInfo, previous *types.Event) (types.EventNID, error) {
	current := previous
	for current != nil {
		if membership, _ := current.Membership(); membership != spec.Join {
			return current.EventNID, nil
		}
		before, err := r.previousMembership(ctx, roomInfo, current.PDU)
		if err != nil {
			return 0, err
		}
		if before == nil {
			return current.EventNID, nil
		}
		if membership, _ := before.Membership(); membership != spec.Join {
			return current.EventNID, nil
		}
		current = before
	}
	return 0, nil
}

// mayRedactFor reports whether the sender of a kick or ban may redact the
// target's events, as MSC4293 requires before the flag takes effect: a power
// level at least that of redact and of m.room.redaction events, if set, or
// a privileged creator.
func (r *Inputer) mayRedactFor(ctx context.Context, roomInfo *types.RoomInfo, membershipEvent gomatrixserverlib.PDU) (bool, error) {
	resolver := state.NewStateResolution(r.DB, roomInfo, r.Queryer)
	if create, err := resolver.CreateEvent(ctx, membershipEvent.EventID()); err == nil && create != nil {
		if verImpl, verErr := gomatrixserverlib.GetRoomVersion(create.Version()); verErr == nil && verImpl.PrivilegedCreators() &&
			slices.Contains(gomatrixserverlib.CreatorsFromCreateEvent(create), string(membershipEvent.SenderID())) {
			return true, nil
		}
	}
	powerLevels, err := resolver.Resolve(ctx, membershipEvent.EventID())
	if err != nil {
		return false, err
	}
	level := powerLevels.UserLevel(membershipEvent.SenderID())
	if level < powerLevels.Redact {
		return false, nil
	}
	if required, ok := powerLevels.Events[spec.MRoomRedaction]; ok && level < required {
		return false, nil
	}
	return true, nil
}

// redactForKickOrBan redacts, after a kick or ban that asks for it, the
// events the target sent during their current stay in the room and stored
// before it, without sending redaction events (MSC4293). Each redacted
// event names the kick or ban as its redacted_because, and downstream
// components are told about it.
func (r *Inputer) redactForKickOrBan(ctx context.Context, roomInfo *types.RoomInfo, eventNID types.EventNID, event gomatrixserverlib.PDU) error {
	redacts, previous, err := r.kickOrBanRedacts(ctx, roomInfo, event)
	if err != nil || !redacts {
		return err
	}
	allowed, err := r.mayRedactFor(ctx, roomInfo, event)
	if err != nil || !allowed {
		return err
	}
	afterNID, err := r.sessionStart(ctx, roomInfo, previous)
	if err != nil {
		return err
	}
	return r.DB.RedactSenderEventsBetween(ctx, roomInfo, spec.SenderID(*event.StateKey()), afterNID, eventNID, event, redactionBatch, func(redacted []string) error {
		outputs := make([]api.OutputEvent, 0, len(redacted))
		for _, eventID := range redacted {
			outputs = append(outputs, api.OutputEvent{
				Type: api.OutputTypeRedactedEvent,
				RedactedEvent: &api.OutputRedactedEvent{
					RedactedEventID: eventID,
					RedactedBecause: &types.HeaderedEvent{PDU: event},
				},
			})
		}
		return r.OutputProducer.ProduceRoomEvents(event.RoomID().String(), outputs)
	})
}

// redactLateEvent redacts an event that arrives after its sender was kicked
// or banned with redact_events, while that is still their membership
// (MSC4293). Such an event is soft-failed, so it is redacted where it is
// stored, without telling downstream components, which never had it.
func (r *Inputer) redactLateEvent(ctx context.Context, roomInfo *types.RoomInfo, eventNID types.EventNID, event gomatrixserverlib.PDU) error {
	if event.Type() == spec.MRoomCreate || event.Type() == spec.MRoomRedaction {
		return nil
	}
	tuple := gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomMember, StateKey: string(event.SenderID())}
	var res api.QueryCurrentStateResponse
	if err := r.Queryer.QueryCurrentState(ctx, &api.QueryCurrentStateRequest{
		RoomID: event.RoomID().String(), StateTuples: []gomatrixserverlib.StateKeyTuple{tuple},
	}, &res); err != nil {
		return err
	}
	membershipEvent := res.StateEvents[tuple]
	if membershipEvent == nil {
		return nil
	}
	redacts, _, err := r.kickOrBanRedacts(ctx, roomInfo, membershipEvent.PDU)
	if err != nil || !redacts {
		return err
	}
	allowed, err := r.mayRedactFor(ctx, roomInfo, membershipEvent.PDU)
	if err != nil || !allowed {
		return err
	}
	return r.DB.RedactEventBecause(ctx, roomInfo, eventNID, membershipEvent.PDU)
}
