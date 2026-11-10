// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"context"
	"strconv"

	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/setup/process"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/streams"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

// StartSyncedEventResponder answers jetstream.RequestSyncedEvent requests,
// which carry jetstream.RoomID and jetstream.EventID headers. It lets another
// component wait until the sync API has caught up with an event it sent to
// the roomserver.
func StartSyncedEventResponder(
	processCtx *process.ProcessContext, natsClient *nats.Conn, topic string,
	db storage.Database, pduStream streams.StreamProvider,
) error {
	ctx := processCtx.Context()
	_, err := natsClient.Subscribe(topic, func(msg *nats.Msg) {
		process.Do(ctx, func() {
			synced := eventSynced(ctx, db, pduStream, msg.Header.Get(jetstream.RoomID), msg.Header.Get(jetstream.EventID))
			res := &nats.Msg{Header: nats.Header{}}
			res.Header.Set(jetstream.Synced, strconv.FormatBool(synced))
			if err := msg.RespondMsg(res); err != nil {
				logrus.WithError(err).Error("Unable to respond to synced event request")
			}
		})
	})
	return err
}

// eventSynced reports whether the event is stored and the PDU stream has
// advanced past it.
func eventSynced(
	ctx context.Context, db storage.Database, pduStream streams.StreamProvider, roomID, eventID string,
) bool {
	if roomID == "" || eventID == "" {
		return false
	}
	snapshot, err := db.NewDatabaseSnapshot(ctx)
	if err != nil {
		return false
	}
	defer snapshot.Rollback() //nolint:errcheck
	pos, _, err := snapshot.SelectContextEvent(ctx, roomID, eventID)
	if err != nil {
		return false
	}
	return types.StreamPosition(pos) <= pduStream.LatestPosition(ctx)
}
