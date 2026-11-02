// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"context"
	"encoding/json"

	"github.com/getsentry/sentry-go"
	"github.com/nats-io/nats.go"
	log "github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/internal/eventutil"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/setup/process"
	"codefloe.com/pat-s/zendrite/syncapi/notifier"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/streams"
	"codefloe.com/pat-s/zendrite/syncapi/types"
)

// OutputProfileUpdateConsumer records the profile changes of local users that
// the user API publishes in the profile updates stream, which the MSC4262
// sliding sync extension reads.
type OutputProfileUpdateConsumer struct {
	ctx       context.Context
	jetstream nats.JetStreamContext
	durable   string
	topic     string
	db        storage.Database
	notifier  *notifier.Notifier
	stream    streams.StreamProvider
}

// NewOutputProfileUpdateConsumer creates a new consumer. Call Start() to
// begin consuming.
func NewOutputProfileUpdateConsumer(
	process *process.ProcessContext,
	cfg *config.SyncAPI,
	js nats.JetStreamContext,
	store storage.Database,
	notifier *notifier.Notifier,
	stream streams.StreamProvider,
) *OutputProfileUpdateConsumer {
	return &OutputProfileUpdateConsumer{
		ctx:       process.Context(),
		jetstream: js,
		durable:   cfg.Matrix.JetStream.Durable("SyncAPIProfileUpdateConsumer"),
		topic:     cfg.Matrix.JetStream.Prefixed(jetstream.OutputProfileUpdate),
		db:        store,
		notifier:  notifier,
		stream:    stream,
	}
}

// Start starts consumption.
func (s *OutputProfileUpdateConsumer) Start() error {
	return jetstream.JetStreamConsumer(
		s.ctx, s.jetstream, s.topic, s.durable, 1,
		s.onMessage, nats.DeliverAll(), nats.ManualAck(),
	)
}

// onMessage is not safe to call from multiple goroutines, or else the stream
// position may race and be incorrectly calculated.
func (s *OutputProfileUpdateConsumer) onMessage(ctx context.Context, msgs []*nats.Msg) bool {
	msg := msgs[0] // Guaranteed to exist if onMessage is called
	userID := msg.Header.Get(jetstream.UserID)

	var update eventutil.ProfileUpdate
	if err := json.Unmarshal(msg.Data, &update); err != nil {
		sentry.CaptureException(err)
		log.WithField("user_id", userID).WithError(err).Error("profile update consumer: message parse failure")
		return true
	}
	if userID == "" || len(update.Fields) == 0 {
		return true
	}

	pos, err := s.db.InsertProfileUpdates(ctx, userID, update.Fields)
	if err != nil {
		sentry.CaptureException(err)
		log.WithField("user_id", userID).WithError(err).Error("Could not save profile update")
		return false
	}

	s.stream.Advance(pos)
	s.notifier.OnNewProfileUpdate(userID, types.StreamingToken{ProfilePosition: pos})
	return true
}
