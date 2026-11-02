// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/internal/sqlutil"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// rejectedRecheckBatchSize is how many rooms the recheck of rejected events
// lists at a time.
const rejectedRecheckBatchSize = 100

// RecheckRejectedEvents checks the rejected memberships and timeline events
// of every room once more, if a migration scheduled it, and accepts those
// that pass against the state as it is stored now.
//
// The state key repair accepted memberships that state stored under the
// wrong key had rejected, and checked the events that depend on them only in
// the pass that accepted them, against partial state. Events whose sender's
// membership it accepted in another pass, and restricted joins authorized by
// a user whose own join it accepted, stayed rejected.
//
// Each room is checked in one room transaction, and progress is recorded per
// room, so an interrupted recheck resumes with the next room on the next
// start.
func (r *Inputer) RecheckRejectedEvents(ctx context.Context) {
	afterRoomNID, pending, err := r.DB.RejectedRecheckCursor(ctx)
	if err != nil {
		logrus.WithError(err).Warn("Failed to read the progress of the rejected event recheck")
		return
	}
	if !pending {
		return
	}
	logrus.WithField("after_room_nid", afterRoomNID).Info("Checking rejected events again")

	rooms, memberships, events, staleRooms, stale := 0, 0, 0, 0, 0
	for {
		if ctx.Err() != nil {
			return
		}
		roomNIDs, err := r.DB.RoomsWithRejectedEventsAfter(ctx, afterRoomNID, rejectedRecheckBatchSize)
		if err != nil {
			logrus.WithError(err).Warn("Failed to list rooms with rejected events")
			return
		}
		for _, roomNID := range roomNIDs {
			roomID, accepted, repaired, acceptedEvents, rerr := r.recheckRoomRejectedEvents(ctx, roomNID)
			if rerr != nil {
				// The room is not recorded, so the next start checks it again.
				logrus.WithError(rerr).WithField("room_nid", roomNID).Warn("Failed to check rejected events again")
				return
			}
			if aerr := r.announceAcceptedTimelineEvents(ctx, roomNID, roomID, acceptedEvents); aerr != nil {
				eventIDs := make([]string, len(acceptedEvents))
				for i := range acceptedEvents {
					eventIDs[i] = acceptedEvents[i].event.EventID()
				}
				logrus.WithError(aerr).WithFields(logrus.Fields{
					"room_id":   roomID,
					"event_ids": eventIDs,
				}).Error("Failed to announce events accepted by the rejected event recheck; clients will not see them")
			}
			if accepted > 0 || len(acceptedEvents) > 0 {
				rooms++
				memberships += accepted
				events += len(acceptedEvents)
				logrus.WithFields(logrus.Fields{
					"room_id":              roomID,
					"accepted_memberships": accepted,
					"accepted_events":      len(acceptedEvents),
				}).Warn("Accepted rejected events that pass against the repaired state")
			}
			if repaired > 0 {
				staleRooms++
				stale += repaired
				logrus.WithFields(logrus.Fields{
					"room_id":     roomID,
					"memberships": repaired,
				}).Warn("Added memberships stored as outliers that follow the current state")
			}
			if err = r.DB.AdvanceRejectedRecheck(ctx, roomNID, false); err != nil {
				logrus.WithError(err).Warn("Failed to record the progress of the rejected event recheck")
				return
			}
			afterRoomNID = roomNID
		}
		if len(roomNIDs) < rejectedRecheckBatchSize {
			break
		}
	}
	if err = r.DB.AdvanceRejectedRecheck(ctx, afterRoomNID, true); err != nil {
		logrus.WithError(err).Warn("Failed to record the end of the rejected event recheck")
		return
	}
	logrus.WithFields(logrus.Fields{
		"rooms":             rooms,
		"memberships":       memberships,
		"events":            events,
		"stale_rooms":       staleRooms,
		"stale_memberships": stale,
	}).Info("Finished checking rejected events again")
}

// recheckRoomRejectedEvents checks the rejected memberships and timeline
// events of a room again in one transaction, accepts those that pass, and
// adds the accepted memberships that pass against the current state to it.
// It then adds the memberships stored as outliers that follow the current
// state to it. It returns the room ID, how many memberships it accepted and
// added, and the timeline events it accepted, which are announced once the
// transaction is committed.
func (r *Inputer) recheckRoomRejectedEvents(
	ctx context.Context, roomNID types.RoomNID,
) (roomID string, accepted, repaired int, acceptedEvents []acceptedTimelineEvent, err error) {
	roomInfo, err := r.DB.RoomInfoByNID(ctx, roomNID)
	if err != nil {
		return "", 0, 0, nil, fmt.Errorf("r.DB.RoomInfoByNID: %w", err)
	}
	if roomInfo == nil || roomInfo.IsStub() {
		return "", 0, 0, nil, nil
	}
	// In a room in partial state the state is partial, and the resync that
	// completes it replaces it.
	partial, err := r.DB.IsRoomPartialState(ctx, roomNID)
	if err != nil {
		return "", 0, 0, nil, fmt.Errorf("r.DB.IsRoomPartialState: %w", err)
	}
	if partial {
		return "", 0, 0, nil, nil
	}
	if roomID, err = r.DB.RoomIDFromNID(ctx, roomNID); err != nil {
		return "", 0, 0, nil, fmt.Errorf("r.DB.RoomIDFromNID: %w", err)
	}

	var succeeded bool
	updater, err := r.DB.GetRoomUpdater(ctx, roomInfo)
	if err != nil {
		return "", 0, 0, nil, fmt.Errorf("r.DB.GetRoomUpdater: %w", err)
	}
	defer sqlutil.EndTransactionWithCheck(updater, &succeeded, &err)

	roomState := state.NewStateResolution(updater, roomInfo, r.Queryer)
	memberKeys := map[types.EventStateKeyNID]struct{}{}
	var updates []api.OutputEvent
	if accepted, acceptedEvents, updates, err = r.acceptRecheckedEvents(ctx, updater, &roomState, roomInfo, nil, memberKeys); err != nil {
		return "", 0, 0, nil, err
	}
	var staleUpdates []api.OutputEvent
	if repaired, staleUpdates, err = r.repairStaleMemberships(ctx, updater, &roomState, roomInfo, memberKeys); err != nil {
		return "", 0, 0, nil, err
	}
	updates = append(updates, staleUpdates...)
	if len(memberKeys) > 0 {
		var membershipUpdates []api.OutputEvent
		if membershipUpdates, err = r.updateRepairedMemberships(ctx, updater, &roomState, roomInfo, memberKeys); err != nil {
			return "", 0, 0, nil, err
		}
		updates = append(updates, membershipUpdates...)
	}
	if len(updates) > 0 {
		if err = r.OutputProducer.ProduceRoomEvents(roomID, updates); err != nil {
			return "", 0, 0, nil, fmt.Errorf("r.OutputProducer.ProduceRoomEvents: %w", err)
		}
	}
	succeeded = true
	return roomID, accepted, repaired, acceptedEvents, nil
}
