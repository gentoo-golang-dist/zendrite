package jetstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
)

type testOwnership struct {
	generation int64
	identity   string
	err        error
}

func (o *testOwnership) Generation() int64 { return o.generation }
func (o *testOwnership) Identity() string  { return o.identity }
func (o *testOwnership) Valid() error      { return o.err }

func TestProtectedPublishStampsTheGeneration(t *testing.T) {
	s := storageTestServer(t, "standalone", nil)
	js := storageTestClient(t, s)
	_, err := js.AddStream(&nats.StreamConfig{Name: "owned", Subjects: []string{"owned"}, Storage: nats.MemoryStorage})
	require.NoError(t, err)

	owner := &testOwnership{generation: 7, identity: "candidate-a"}
	protected := protectedJetStream{JetStreamContext: js, ownership: owner}
	_, err = protected.PublishMsg(&nats.Msg{Subject: "owned", Data: []byte("event")})
	require.NoError(t, err)

	sub, err := js.PullSubscribe("owned", "reader")
	require.NoError(t, err)
	msgs, err := sub.Fetch(1, nats.MaxWait(2*time.Second))
	require.NoError(t, err)
	require.Equal(t, "7", msgs[0].Header.Get(GenerationHeader))
}

func TestProtectedPublishRefusesARetiredOwner(t *testing.T) {
	s := storageTestServer(t, "standalone", nil)
	js := storageTestClient(t, s)
	_, err := js.AddStream(&nats.StreamConfig{Name: "owned", Subjects: []string{"owned"}, Storage: nats.MemoryStorage})
	require.NoError(t, err)

	retired := errors.New("ownership generation is retired")
	protected := protectedJetStream{JetStreamContext: js, ownership: &testOwnership{generation: 7, err: retired}}
	_, err = protected.PublishMsg(&nats.Msg{Subject: "owned", Data: []byte("stale")})
	require.ErrorIs(t, err, retired)

	info, err := js.StreamInfo("owned")
	require.NoError(t, err)
	require.Zero(t, info.State.Msgs, "a retired owner's message must never reach the stream")
}

func TestHAConnectionIsNamedAndDoesNotBufferOnReconnect(t *testing.T) {
	cfg := &config.JetStream{Matrix: &config.Global{}}
	cfg.Matrix.HA.Enabled = true
	owner := &testOwnership{generation: 7, identity: "candidate-a"}

	opts, err := natsConnectOptions(nil, cfg, nil, owner)
	require.NoError(t, err)
	var applied nats.Options
	for _, opt := range opts {
		require.NoError(t, opt(&applied))
	}
	require.Contains(t, applied.Name, "candidate-a", "the connection must identify its activation")
	require.Negative(t, applied.ReconnectBufSize, "a resumed owner must not flush buffered publishes")
}

// TestConsumerDoesNotAcknowledgeAfterOwnershipLoss covers ADR 0003's
// acknowledgement rule. A retired owner that acknowledges work it did not
// complete would discard it, so the consumer must stop instead, leaving the
// message pending for the promoted activation.
func TestConsumerDoesNotAcknowledgeAfterOwnershipLoss(t *testing.T) {
	s := storageTestServer(t, "standalone", nil)
	js := storageTestClient(t, s)
	_, err := js.AddStream(&nats.StreamConfig{Name: "owned", Subjects: []string{"owned"}, Storage: nats.MemoryStorage, Retention: nats.LimitsPolicy})
	require.NoError(t, err)
	_, err = js.Publish("owned", []byte("work"))
	require.NoError(t, err)

	retired := errors.New("ownership generation is retired")
	owner := &testOwnership{generation: 7, identity: "candidate-a"}
	protected := protectedJetStream{JetStreamContext: js, ownership: owner}

	ctx, cancel := context.WithCancelCause(t.Context())
	t.Cleanup(func() { cancel(errors.New("test finished")) })
	delivered := make(chan struct{}, 1)
	require.NoError(t, JetStreamConsumer(ctx, protected, "owned", "worker", 1,
		func(context.Context, []*nats.Msg) bool {
			// Ownership is lost between doing the work and acknowledging it.
			owner.err = retired
			select {
			case delivered <- struct{}{}:
			default:
			}
			return true
		}))

	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer never received the message")
	}

	require.Eventually(t, func() bool {
		info, infoErr := js.ConsumerInfo("owned", "workerPull")
		return infoErr == nil && info.AckFloor.Consumer == 0 && info.NumAckPending == 1
	}, 5*time.Second, 50*time.Millisecond, "a retired owner must leave the message unacknowledged")
}
