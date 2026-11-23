package jetstream

import (
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/process"
)

// manyConsumers is one more consumer than a NATS 2.15 server allows on a
// stream by default.
const manyConsumers = 1001

// addRoomConsumers creates n durable consumers on a stream, as the
// roomserver does for each active room.
func addRoomConsumers(t *testing.T, js nats.JetStreamContext, stream string, n int) {
	t.Helper()
	for i := range n {
		_, err := js.AddConsumer(stream, &nats.ConsumerConfig{
			Durable: fmt.Sprintf("RoomInput%d", i), AckPolicy: nats.AckExplicitPolicy,
			FilterSubject: fmt.Sprintf("%s.room%d", stream, i),
		})
		require.NoError(t, err, "consumer %d", i)
	}
}

// TestEmbeddedServerAllowsManyConsumers covers issue #745: the embedded
// server applied the 1000 consumer default to every stream, so a server with
// more active rooms could neither create rooms nor restart.
func TestEmbeddedServerAllowsManyConsumers(t *testing.T) {
	global := &config.Global{JetStream: config.JetStream{TopicPrefix: "Limit_", StoragePath: config.Path(t.TempDir()), NoLog: true}}
	global.JetStream.Matrix = global
	p := process.NewProcessContextWithParent(t.Context())
	t.Cleanup(func() { p.ShutdownZendrite(); p.WaitForComponentsToFinish() })
	js, _ := (&NATSInstance{}).Prepare(p, &global.JetStream)

	// A stream without a limit of its own, as one an older release created.
	_, err := js.AddStream(&nats.StreamConfig{Name: "Unlimited", Subjects: []string{"Unlimited.>"}})
	require.NoError(t, err)
	addRoomConsumers(t, js, "Unlimited", manyConsumers)
}

// TestStreamsAllowManyConsumers covers an external server, whose default
// consumer limit Zendrite cannot change: the streams must carry a limit of
// their own, including streams created before they did.
func TestStreamsAllowManyConsumers(t *testing.T) {
	s := storageTestServer(t, "standalone", nil)
	js := storageTestClient(t, s)
	cfg := &config.JetStream{TopicPrefix: "limit_", Replicas: 1}
	name := cfg.Prefixed(InputRoomEvent)
	_, err := js.AddStream(&nats.StreamConfig{Name: name, Subjects: []string{name, name + ".>"}, Storage: nats.FileStorage, Retention: nats.InterestPolicy, MaxAge: streams[0].MaxAge})
	require.NoError(t, err)
	p := process.NewProcessContextWithParent(t.Context())
	require.NoError(t, configureStreams(p, cfg, js))

	for _, template := range streams {
		info, infoErr := js.StreamInfo(cfg.Prefixed(template.Name))
		require.NoError(t, infoErr)
		require.Equal(t, streamMaxConsumers, info.Config.MaxConsumers, template.Name)
	}
	addRoomConsumers(t, js, name, manyConsumers)
}

func TestStreamsKeepOperatorConsumerLimit(t *testing.T) {
	cfg := &config.JetStream{TopicPrefix: "operator_"}
	js := &streamConfigRecorder{configs: make(map[string]nats.StreamConfig)}
	name := cfg.Prefixed(InputRoomEvent)
	existing := *streams[0]
	existing.Name = name
	existing.Subjects = []string{name, name + ".>"}
	existing.Replicas = 1
	existing.MaxConsumers = 5000
	js.configs[name] = existing
	p := process.NewProcessContextWithParent(t.Context())
	require.NoError(t, configureStreams(p, cfg, js))
	require.Equal(t, 5000, js.configs[name].MaxConsumers)
}

// accountLimitedStreams refuses any stream limit above an account's
// consumer limit, as a NATS server does.
type accountLimitedStreams struct {
	streamConfigRecorder
	accountMaxConsumers int
}

func (a *accountLimitedStreams) refuse(cfg *nats.StreamConfig) error {
	if cfg.MaxConsumers > a.accountMaxConsumers {
		return &nats.APIError{Code: 400, ErrorCode: jsErrCodeMaximumConsumersLimit, Description: "maximum consumers limit reached"}
	}
	return nil
}

func (a *accountLimitedStreams) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if err := a.refuse(cfg); err != nil {
		return nil, err
	}
	return a.streamConfigRecorder.AddStream(cfg, opts...)
}

func (a *accountLimitedStreams) UpdateStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	if err := a.refuse(cfg); err != nil {
		return nil, err
	}
	return a.streamConfigRecorder.UpdateStream(cfg, opts...)
}

// TestStreamsFallBackToAccountConsumerLimit covers an account that caps
// consumers: the stream limit is refused, and startup must not fail over it.
func TestStreamsFallBackToAccountConsumerLimit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing bool
		replicas int
	}{
		{name: "new stream"},
		{name: "existing stream", existing: true},
		{name: "existing stream with another update", existing: true, replicas: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.JetStream{TopicPrefix: "account_", Replicas: tc.replicas}
			js := &accountLimitedStreams{streamConfigRecorder: streamConfigRecorder{configs: make(map[string]nats.StreamConfig)}, accountMaxConsumers: 100}
			name := cfg.Prefixed(InputRoomEvent)
			if tc.existing {
				existing := *streams[0]
				existing.Name = name
				existing.Subjects = []string{name, name + ".>"}
				existing.Replicas = 1
				js.configs[name] = existing
			}
			p := process.NewProcessContextWithParent(t.Context())
			require.NoError(t, configureStreams(p, cfg, js))
			require.Zero(t, js.configs[name].MaxConsumers)
			if tc.replicas > 0 {
				require.Equal(t, tc.replicas, js.configs[name].Replicas)
			}
		})
	}
}

func TestIsConsumerLimitError(t *testing.T) {
	s := storageTestServer(t, "standalone", nil)
	js := storageTestClient(t, s)
	_, err := js.AddStream(&nats.StreamConfig{Name: "capped", Subjects: []string{"capped.>"}, MaxConsumers: 1})
	require.NoError(t, err)
	addRoomConsumers(t, js, "capped", 1)
	_, err = js.AddConsumer("capped", &nats.ConsumerConfig{Durable: "over", AckPolicy: nats.AckExplicitPolicy})
	require.True(t, IsConsumerLimitError(fmt.Errorf("create consumer: %w", err)), "%v", err)
	require.False(t, IsConsumerLimitError(nats.ErrConsumerNotFound))
	require.False(t, IsConsumerLimitError(nil))
}
