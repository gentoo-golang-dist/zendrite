package streams

import (
	"context"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/internal/eventutil"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

type NotificationDataStreamProvider struct {
	DefaultStreamProvider
}

func (p *NotificationDataStreamProvider) Setup(
	ctx context.Context, snapshot storage.DatabaseTransaction,
) {
	p.DefaultStreamProvider.Setup(ctx, snapshot)

	p.latestMutex.Lock()
	defer p.latestMutex.Unlock()

	id, err := snapshot.MaxStreamPositionForNotificationData(ctx)
	if err != nil {
		panic(err)
	}
	p.latest = id
}

func (p *NotificationDataStreamProvider) CompleteSync(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	req *types.SyncRequest,
) types.StreamPosition {
	return p.IncrementalSync(ctx, snapshot, req, 0, p.LatestPosition(ctx))
}

func (p *NotificationDataStreamProvider) IncrementalSync(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	req *types.SyncRequest,
	from, to types.StreamPosition,
) types.StreamPosition {
	// Counts change without other activity, e.g. after a read receipt, which
	// can reach the client before its effect on the counts.
	// Such rooms are sent with only their counts.
	if from > 0 {
		updated, err := snapshot.RoomsWithUpdatedNotificationCounts(ctx, req.Device.UserID, from, to)
		if err != nil {
			req.Log.WithError(err).Error("RoomsWithUpdatedNotificationCounts failed")
			return from
		}
		for _, roomID := range updated {
			if req.Rooms[roomID] != spec.Join {
				continue
			}
			jr, ok := req.Response.Rooms.Join[roomID]
			if !ok {
				jr = types.NewJoinResponse()
			}
			jr.NotificationsChanged = true
			req.Response.Rooms.Join[roomID] = jr
		}
	}

	// Get the unread notifications for rooms in our join response.
	// This is to ensure clients always have an unread notification section
	// and can display the correct numbers.
	countsByRoom, err := snapshot.GetUserUnreadNotificationCountsForRooms(ctx, req.Device.UserID, req.Rooms)
	if err != nil {
		req.Log.WithError(err).Error("GetUserUnreadNotificationCountsForRooms failed")
		return from
	}

	// With per-thread counts, the room's counts cover only the main timeline.
	byThread := req.Filter.Room.Timeline.UnreadThreadNotifications

	// We're merely decorating existing rooms.
	for roomID, jr := range req.Response.Rooms.Join {
		counts := countsByRoom[roomID]
		if counts == nil {
			counts = &eventutil.NotificationData{}
		}
		jr.UnreadNotifications = &types.UnreadNotifications{
			HighlightCount:    counts.UnreadHighlightCount,
			NotificationCount: counts.UnreadNotificationCount,
		}
		if byThread {
			main, threads := counts.SplitByThread()
			jr.UnreadNotifications = &types.UnreadNotifications{
				HighlightCount:    main.UnreadHighlightCount,
				NotificationCount: main.UnreadNotificationCount,
			}
			jr.UnreadThreadNotifications = make(map[string]*types.UnreadNotifications, len(threads))
			for threadID, thread := range threads {
				jr.UnreadThreadNotifications[threadID] = &types.UnreadNotifications{
					HighlightCount:    thread.UnreadHighlightCount,
					NotificationCount: thread.UnreadNotificationCount,
				}
			}
		}
		req.Response.Rooms.Join[roomID] = jr
	}

	// Changes after to are found by the next sync.
	return to
}
