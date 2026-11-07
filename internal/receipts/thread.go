package receipts

import (
	"context"
	"errors"
	"slices"

	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// EventQuerier retrieves events for receipt validation and notification classification.
type EventQuerier interface {
	QueryEventsByID(context.Context, *api.QueryEventsByIDRequest, *api.QueryEventsByIDResponse) error
}

// ErrUnknownEvent means the acknowledged event is unavailable in the requested room.
var ErrUnknownEvent = errors.New("receipt event is not in the room")

// ThreadID resolves an event's thread through at most three relationship hops.
// Thread roots and non-thread relations to roots belong to the main timeline.
func ThreadID(ctx context.Context, query EventQuerier, roomID, eventID string) (string, error) {
	event, err := queryEvent(ctx, query, roomID, eventID)
	if err != nil {
		return "", err
	}
	return EventThreadID(ctx, query, event)
}

// EventThreadID classifies an event already available to the caller.
func EventThreadID(ctx context.Context, query EventQuerier, event *types.HeaderedEvent) (string, error) {
	threadID, _, err := relationChain(ctx, query, event)
	return threadID, err
}

// RelatedToThread reports whether a receipt for threadID may acknowledge the event.
// That is the case when the event is in the thread, or when it relates to the
// thread root through other relations, like a reaction to a reference to the root.
// The root itself is not part of its thread.
func RelatedToThread(ctx context.Context, query EventQuerier, roomID, eventID, threadID string) (bool, error) {
	event, err := queryEvent(ctx, query, roomID, eventID)
	if err != nil {
		return false, err
	}
	actual, parents, err := relationChain(ctx, query, event)
	if err != nil {
		return false, err
	}
	if actual == threadID {
		return true, nil
	}
	return threadID != "main" && actual == "main" && slices.Contains(parents, threadID), nil
}

// relationChain follows an event's relations through at most three hops.
// It returns the event's thread and the parents passed on the way.
func relationChain(ctx context.Context, query EventQuerier, event *types.HeaderedEvent) (threadID string, parents []string, err error) {
	roomID := event.RoomID().String()
	seen := make(map[string]bool)
	for range 3 {
		if seen[event.EventID()] {
			break
		}
		seen[event.EventID()] = true
		relation := gjson.GetBytes(event.Content(), "m\\.relates_to")
		parentID := relation.Get("event_id").Str
		if parentID == "" {
			break
		}
		if relation.Get("rel_type").Str == "m.thread" {
			return parentID, parents, nil
		}
		parents = append(parents, parentID)
		parent, err := queryEvent(ctx, query, roomID, parentID)
		if errors.Is(err, ErrUnknownEvent) {
			break
		}
		if err != nil {
			return "", nil, err
		}
		event = parent
	}
	return "main", parents, nil
}

func queryEvent(ctx context.Context, query EventQuerier, roomID, eventID string) (*types.HeaderedEvent, error) {
	var res api.QueryEventsByIDResponse
	if err := query.QueryEventsByID(ctx, &api.QueryEventsByIDRequest{RoomID: roomID, EventIDs: []string{eventID}, ExcludeRejected: true}, &res); err != nil {
		return nil, err
	}
	for _, event := range res.Events {
		if event.EventID() == eventID && event.RoomID().String() == roomID {
			return event, nil
		}
	}
	return nil, ErrUnknownEvent
}
