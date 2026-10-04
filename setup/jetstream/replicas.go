package jetstream

import (
	"context"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/process"
)

// The replica metrics are labeled by the unprefixed stream name, a fixed set,
// and by the broker's server name, one per broker in the cluster.

// streamReplicaCurrent is 1 while a stream's follower is current with its
// leader, and 0 while it is behind or offline.
var streamReplicaCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "zendrite", Subsystem: "jetstream", Name: "stream_replica_current",
	Help: "1 while a stream follower is current with its leader, 0 while it is behind or offline.",
}, []string{"stream", "replica"})

// streamReplicaLag is how many entries a stream's follower is behind its
// leader, as the leader reports it.
var streamReplicaLag = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "zendrite", Subsystem: "jetstream", Name: "stream_replica_lag",
	Help: "Entries a stream follower is behind its leader, as the leader reports.",
}, []string{"stream", "replica"})

func init() { //nolint:gochecknoinits // Matches the existing component metric registration.
	prometheus.MustRegister(streamReplicaCurrent, streamReplicaLag)
}

// defaultReplicaCheckInterval is how often stream replicas are read from the
// broker.
const defaultReplicaCheckInterval = 30 * time.Second

// replicaStuckAfter is how long a follower may stay behind before it is
// reported. A follower falls behind briefly under load and while its broker
// restarts, and catches up within seconds once the broker is back.
const replicaStuckAfter = 5 * time.Minute

// replicaMonitor reports stream followers that stay behind their leader.
//
// A nats-server broker that rejoined on its retained storage can stop applying
// a stream, logging "expected sequence does not match store" (#473).
// The stream keeps its leader and a current majority, so nothing fails, but
// it is one broker loss away from losing its quorum, and only an operator can
// repair the replica. The leader's view of each follower is the one signal a
// client can read, so it is exported and a stuck follower is logged.
type replicaMonitor struct {
	stuckAfter time.Duration

	mu sync.Mutex
	// behind holds when each stream/replica pair was first seen behind, and
	// whether that has been reported.
	behind map[replicaKey]*replicaBehind
}

type replicaKey struct{ stream, replica string }

type replicaBehind struct {
	since    time.Time
	reported bool
}

func newReplicaMonitor() *replicaMonitor {
	return &replicaMonitor{stuckAfter: replicaStuckAfter, behind: map[replicaKey]*replicaBehind{}}
}

// monitorReplicas reads every stream's replicas from the broker until the
// process ends. It does nothing for unreplicated streams.
func monitorReplicas(proc *process.ProcessContext, cfg *config.JetStream, js nats.JetStreamContext, interval time.Duration) {
	if cfg.Replicas < 2 { //nolint:mnd // A stream needs a follower to have one behind.
		return
	}
	m := newReplicaMonitor()
	ctx := proc.Context()
	for process.Sleep(ctx, interval) {
		for _, stream := range streams {
			reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second) //nolint:mnd
			info, err := js.StreamInfo(cfg.Prefixed(stream.Name), nats.Context(reqCtx))
			cancel()
			if err != nil {
				// A stream without a leader cannot answer; the publication and
				// consumer metrics show that outage.
				continue
			}
			m.observe(time.Now(), stream.Name, info.Cluster)
		}
	}
}

// observe records one reading of a stream's followers.
func (m *replicaMonitor) observe(now time.Time, stream string, cluster *nats.ClusterInfo) {
	if cluster == nil || cluster.Leader == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// A replaced broker can rejoin under another name, so drop the series of
	// peers that are no longer followers.
	streamReplicaCurrent.DeletePartialMatch(prometheus.Labels{"stream": stream})
	streamReplicaLag.DeletePartialMatch(prometheus.Labels{"stream": stream})
	seen := map[string]bool{}
	for _, peer := range cluster.Replicas {
		seen[peer.Name] = true
		current := peer.Current && !peer.Offline
		value := 0.0
		if current {
			value = 1
		}
		streamReplicaCurrent.WithLabelValues(stream, peer.Name).Set(value)
		streamReplicaLag.WithLabelValues(stream, peer.Name).Set(float64(peer.Lag))

		key := replicaKey{stream, peer.Name}
		b := m.behind[key]
		switch {
		case current && b != nil:
			if b.reported {
				logrus.WithFields(logrus.Fields{
					"stream": stream, "replica": peer.Name, "leader": cluster.Leader,
					"behind_for": now.Sub(b.since).Round(time.Second).String(),
				}).Info("JetStream stream replica caught up with its leader")
			}
			delete(m.behind, key)
		case current:
		case b == nil:
			m.behind[key] = &replicaBehind{since: now}
		case !b.reported && now.Sub(b.since) >= m.stuckAfter:
			b.reported = true
			logrus.WithFields(logrus.Fields{
				"stream": stream, "replica": peer.Name, "leader": cluster.Leader,
				"lag": peer.Lag, "offline": peer.Offline,
				"behind_for": now.Sub(b.since).Round(time.Second).String(),
			}).Warn("JetStream stream replica is not catching up with its leader; one more broker loss would leave the stream without a quorum. " +
				"If the broker is running, it may have stopped applying the stream: check its log for \"critical write error\" and restart it")
		}
	}
	for key := range m.behind {
		if key.stream == stream && !seen[key.replica] {
			delete(m.behind, key)
		}
	}
}
