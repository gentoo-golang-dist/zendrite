package input

import (
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/setup/process"
)

// startConsumerLimitedInput runs an Inputer's announcements against an input
// stream whose consumer limit is already reached by the announcer and one
// other room's consumer, as on a server with more active rooms than the
// broker's consumer limit allows (issue #745).
func startConsumerLimitedInput(t *testing.T) (*Inputer, *nats.Conn, nats.JetStreamContext) {
	t.Helper()
	s, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoSigs: true, NoLog: true,
	})
	require.NoError(t, err)
	s.Start()
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	require.True(t, s.ReadyForConnections(5*time.Second))
	nc, err := nats.Connect(s.ClientURL())
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := nc.JetStream()
	require.NoError(t, err)

	cfg := &config.RoomServer{Matrix: &config.Global{JetStream: config.JetStream{TopicPrefix: "Test"}}}
	stream := cfg.Matrix.JetStream.Prefixed(jetstream.InputRoomEvent)
	_, err = js.AddStream(&nats.StreamConfig{
		Name: stream, Subjects: []string{stream + ".>"}, Retention: nats.InterestPolicy, MaxConsumers: 2,
	})
	require.NoError(t, err)
	_, err = js.AddConsumer(stream, &nats.ConsumerConfig{
		Durable: "OtherRoom", AckPolicy: nats.AckExplicitPolicy, FilterSubject: stream + ".other",
	})
	require.NoError(t, err)

	retryDelay := consumerSetupRetryDelay
	consumerSetupRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { consumerSetupRetryDelay = retryDelay })
	p := process.NewProcessContextWithParent(t.Context())
	t.Cleanup(func() { p.ShutdownZendrite(); p.WaitForComponentsToFinish() })
	r := &Inputer{Cfg: cfg, ProcessContext: p, JetStream: js, NATSClient: nc, InputRoomEventTopic: stream}
	sub, err := r.subscribeAnnouncements()
	require.NoError(t, err)
	p.Go(func() { r.watchAnnouncements(sub) })
	return r, nc, js
}

// TestSyncInputFailsWhenConsumerLimitReached covers createRoom hanging for a
// minute and then failing without a reason: a synchronous caller must be told
// promptly why its input cannot be processed.
func TestSyncInputFailsWhenConsumerLimitReached(t *testing.T) {
	r, nc, js := startConsumerLimitedInput(t)
	roomID := "!new:test"

	inbox := nats.NewInbox()
	replies, err := nc.SubscribeSync(inbox)
	require.NoError(t, err)
	msg := nats.NewMsg(r.Cfg.Matrix.JetStream.Prefixed(jetstream.InputRoomEventSubj(roomID)))
	msg.Header.Set(jetstream.RoomID, roomID)
	msg.Header.Set("sync", inbox)
	msg.Data = []byte("not an input event")
	_, err = js.PublishMsg(msg)
	require.NoError(t, err)

	reply, err := replies.NextMsg(10 * time.Second)
	require.NoError(t, err, "the synchronous caller was never answered")
	require.Contains(t, string(reply.Data), "consumer limit")

	// The caller was told the input failed, so the event is not processed later.
	require.Eventually(t, func() bool {
		info, infoErr := js.StreamInfo(r.InputRoomEventTopic)
		return infoErr == nil && info.State.Msgs == 0
	}, 5*time.Second, 20*time.Millisecond, "the failed input was left in the stream")
}

// TestAsyncInputWaitsForConsumerLimit covers federated input arriving while
// the consumer limit is reached: no one waits for it, so it is kept and
// processed once the limit allows the room's consumer.
func TestAsyncInputWaitsForConsumerLimit(t *testing.T) {
	r, _, js := startConsumerLimitedInput(t)
	roomID := "!federated:test"
	seq := queueRoomInput(t, r, js, roomID)

	time.Sleep(200 * time.Millisecond)
	info, err := js.StreamInfo(r.InputRoomEventTopic)
	require.NoError(t, err)
	require.EqualValues(t, 1, info.State.Msgs, "asynchronous input must be kept while the room has no consumer")

	cfg := info.Config
	cfg.MaxConsumers = 3
	_, err = js.UpdateStream(&cfg)
	require.NoError(t, err)
	requireRoomFetched(t, r, js, roomID, seq, "the held input was not processed once the limit allowed it")
}
