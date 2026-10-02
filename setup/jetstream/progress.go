package jetstream

import (
	"context"
	"time"

	"github.com/nats-io/nats.go"

	"codefloe.com/pat-s/zendrite/setup/process"
)

// consumerStatsInterval is how often a consumer's backlog is read from the
// broker. Tests shorten it.
var consumerStatsInterval = 30 * time.Second

// monitorBacklog refreshes consumer_pending_messages from the broker until
// ctx ends.
//
// The worker updates the gauge from each delivery's metadata, so a worker that
// stopped fetching, or one whose pulls keep failing, would leave it at its last
// value, often zero, while messages pile up. Reading the backlog from the
// broker keeps it current, so a growing backlog with an old
// consumer_last_ack_timestamp_seconds reveals the stall. A failed read, such
// as during a leader election, leaves the last value standing.
func monitorBacklog(ctx context.Context, sub *nats.Subscription, durable string) {
	pending := consumerPending.WithLabelValues(durable)
	for process.Sleep(ctx, consumerStatsInterval) {
		if info, err := sub.ConsumerInfo(); err == nil {
			pending.Set(float64(info.NumPending))
		}
	}
}

// countFetchError records a failed pull request of a durable consumer.
func countFetchError(durable string) {
	consumerFetchErrors.WithLabelValues(durable).Inc()
}
