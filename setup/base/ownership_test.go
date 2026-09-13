package base

import (
	"context"
	"errors"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
)

type testOwner struct{ err error }

func (o *testOwner) Generation() int64 { return 1 }
func (o *testOwner) Identity() string  { return "candidate-a" }
func (o *testOwner) Valid() error      { return o.err }

type recordingFederationClient struct {
	fclient.FederationClient
	sent int
}

func (c *recordingFederationClient) SendTransaction(context.Context, gomatrixserverlib.Transaction) (fclient.RespSend, error) {
	c.sent++
	return fclient.RespSend{}, nil
}

func (c *recordingFederationClient) P2PSendTransactionToRelay(context.Context, spec.UserID, gomatrixserverlib.Transaction, spec.ServerName) (fclient.EmptyResp, error) {
	c.sent++
	return fclient.EmptyResp{}, nil
}

func TestGuardedFederationClientRefusesDispatchWithoutOwnership(t *testing.T) {
	retired := errors.New("ownership generation is retired")
	inner := &recordingFederationClient{}
	guarded := GuardFederationClient(inner, &testOwner{err: retired})

	_, err := guarded.SendTransaction(t.Context(), gomatrixserverlib.Transaction{})
	require.ErrorIs(t, err, retired)
	user, err := spec.NewUserID("@alice:example.com", true)
	require.NoError(t, err)
	_, err = guarded.P2PSendTransactionToRelay(t.Context(), *user, gomatrixserverlib.Transaction{}, "relay.example.com")
	require.ErrorIs(t, err, retired)
	require.Zero(t, inner.sent, "a retired activation must not start a new outbound transaction")
}

func TestGuardedFederationClientDispatchesWhileOwned(t *testing.T) {
	inner := &recordingFederationClient{}
	guarded := GuardFederationClient(inner, &testOwner{})

	_, err := guarded.SendTransaction(t.Context(), gomatrixserverlib.Transaction{})
	require.NoError(t, err)
	require.Equal(t, 1, inner.sent)
}
