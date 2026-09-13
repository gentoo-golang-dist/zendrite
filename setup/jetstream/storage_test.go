package jetstream

import (
	"fmt"
	"net/url"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/process"
)

func storageTestServer(t *testing.T, name string, routes []*url.URL) *natsserver.Server {
	t.Helper()
	opts := &natsserver.Options{ServerName: name, Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoSigs: true, NoLog: true, SyncAlways: true}
	if name != "standalone" {
		opts.Cluster = natsserver.ClusterOpts{Name: "ha-storage-test", Host: "127.0.0.1", Port: -1}
		opts.Routes = routes
		if len(routes) == 0 {
			opts.Routes = []*url.URL{{Scheme: "nats", Host: "127.0.0.1:1"}}
		}
	}
	s, err := natsserver.NewServer(opts)
	require.NoError(t, err)
	s.Start()
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	require.True(t, s.ReadyForConnections(10*time.Second))
	return s
}

func storageTestClient(t *testing.T, s *natsserver.Server) nats.JetStreamContext {
	t.Helper()
	nc, err := nats.Connect(s.ClientURL(), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := nc.JetStream(nats.MaxWait(2 * time.Second))
	require.NoError(t, err)
	return js
}

func TestStreamUpdatePreservesNamespaceAndPendingData(t *testing.T) {
	s := storageTestServer(t, "standalone", nil)
	js := storageTestClient(t, s)
	cfg := &config.JetStream{TopicPrefix: "custom_", Replicas: 1}
	name := cfg.Prefixed(InputRoomEvent)
	_, err := js.AddStream(&nats.StreamConfig{Name: name, Subjects: []string{name, name + ".>"}, Storage: nats.FileStorage, Retention: nats.InterestPolicy, MaxAge: 48 * time.Hour, MaxBytes: 1024 * 1024})
	require.NoError(t, err)
	_, err = js.AddConsumer(name, &nats.ConsumerConfig{Durable: "pending", AckPolicy: nats.AckExplicitPolicy})
	require.NoError(t, err)
	ack, err := js.Publish(name, []byte("preserve me"))
	require.NoError(t, err)
	p := process.NewProcessContextWithParent(t.Context())
	require.NoError(t, configureStreams(p, cfg, js))
	info, err := js.StreamInfo(name)
	require.NoError(t, err)
	require.Equal(t, 24*time.Hour, info.Config.MaxAge)
	require.EqualValues(t, 1024*1024, info.Config.MaxBytes)
	msg, err := js.GetMsg(name, ack.Sequence)
	require.NoError(t, err)
	require.Equal(t, "preserve me", string(msg.Data))
	_, err = js.StreamInfo(InputRoomEvent)
	require.ErrorIs(t, err, nats.ErrStreamNotFound)
	// An unsupported storage conversion must leave the stream and message intact.
	cfg.InMemory = true
	require.Error(t, configureStreams(p, cfg, js))
	_, err = js.GetMsg(name, ack.Sequence)
	require.NoError(t, err)
	require.Equal(t, nats.FileStorage, streams[0].Storage)
}

func TestHAInsufficientReplicasDoesNotFallBack(t *testing.T) {
	s := storageTestServer(t, "standalone", nil)
	js := storageTestClient(t, s)
	cfg := &config.JetStream{Matrix: &config.Global{HA: config.HA{Enabled: true}}, TopicPrefix: "ha_", Replicas: 3}
	p := process.NewProcessContextWithParent(t.Context())
	require.Error(t, configureStreams(p, cfg, js))
	_, err := js.StreamInfo(cfg.Prefixed(InputRoomEvent))
	require.ErrorIs(t, err, nats.ErrStreamNotFound)
	degraded, _ := p.IsDegraded()
	require.False(t, degraded)
}

func TestHAReplicationUpgradeAndBrokerFailover(t *testing.T) {
	seed := storageTestServer(t, "node-0", nil)
	route, err := url.Parse("nats://" + seed.ClusterAddr().String())
	require.NoError(t, err)
	servers := []*natsserver.Server{seed}
	for i := 1; i < 3; i++ {
		servers = append(servers, storageTestServer(t, fmt.Sprintf("node-%d", i), []*url.URL{route}))
	}
	js := storageTestClient(t, seed)
	cfg := &config.JetStream{Matrix: &config.Global{HA: config.HA{Enabled: true}}, TopicPrefix: "ha_", Replicas: 3}
	// Existing R1 streams must be upgraded, including streams with pending work.
	for _, template := range streams {
		desired := *template
		desired.Name = cfg.Prefixed(template.Name)
		desired.Subjects = []string{desired.Name, desired.Name + ".>"}
		desired.Storage = nats.FileStorage
		desired.Replicas = 1
		require.Eventually(t, func() bool { _, addErr := js.AddStream(&desired); return addErr == nil }, 15*time.Second, 100*time.Millisecond)
	}
	name := cfg.Prefixed(InputRoomEvent)
	_, err = js.AddConsumer(name, &nats.ConsumerConfig{Durable: "pending", AckPolicy: nats.AckExplicitPolicy})
	require.NoError(t, err)
	ack, err := js.Publish(name, []byte("acknowledged"))
	require.NoError(t, err)
	p := process.NewProcessContextWithParent(t.Context())
	require.NoError(t, configureStreams(p, cfg, js))
	// Also exercise fresh R3 creation without replacing any occupied stream.
	emptyName := cfg.Prefixed(OutputTypingEvent)
	require.NoError(t, js.DeleteStream(emptyName))
	require.NoError(t, configureStreams(p, cfg, js))
	info, err := js.StreamInfo(name)
	require.NoError(t, err)
	for _, server := range servers {
		if server.Name() == info.Cluster.Leader {
			server.Shutdown()
			server.WaitForShutdown()
			break
		}
	}
	require.Eventually(t, func() bool {
		msg, getErr := js.GetMsg(name, ack.Sequence)
		return getErr == nil && string(msg.Data) == "acknowledged"
	}, 15*time.Second, 100*time.Millisecond)
	consumer, err := js.ConsumerInfo(name, "pending")
	require.NoError(t, err)
	require.EqualValues(t, 1, consumer.NumPending)
}

func TestSyncRecoveryWaitsForUnacknowledgedMessages(t *testing.T) {
	s := storageTestServer(t, "standalone", nil)
	js := storageTestClient(t, s)
	cfg := &config.JetStream{TopicPrefix: "sync_"}
	p := process.NewProcessContextWithParent(t.Context())
	require.NoError(t, configureStreams(p, cfg, js))
	for _, pair := range syncRecoveryConsumers {
		_, err := js.AddConsumer(cfg.Prefixed(pair.stream), &nats.ConsumerConfig{Durable: cfg.Durable(pair.consumer) + "Pull", AckPolicy: nats.AckExplicitPolicy})
		require.NoError(t, err)
	}
	pair := syncRecoveryConsumers[0]
	stream, durable := cfg.Prefixed(pair.stream), cfg.Durable(pair.consumer)+"Pull"
	_, err := js.Publish(stream, []byte("must apply before ready"))
	require.NoError(t, err)
	sub, err := js.PullSubscribe(stream, durable, nats.Bind(stream, durable))
	require.NoError(t, err)
	msgs, err := sub.Fetch(1, nats.Context(t.Context()))
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- RecoverSync(t.Context(), js, cfg) }()
	select {
	case err := <-done:
		t.Fatalf("recovery finished before acknowledgement: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	require.NoError(t, msgs[0].AckSync())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not complete")
	}
}
