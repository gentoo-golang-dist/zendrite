package ha

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	basepkg "codefloe.com/pat-s/zendrite/setup/base"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/setup/lifecycle"
	"codefloe.com/pat-s/zendrite/test"
)

// takeover promotes a first activation, then a successor, and returns both
// gates. The first gate is left as a resumed owner would be: still running,
// still connected, and not yet aware that it was replaced.
func takeover(t *testing.T, retiredCfg, promotedCfg config.HA) (retired, promoted *PostgresGate, dsn string) {
	t.Helper()
	dsn, cleanup := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	t.Cleanup(cleanup)
	scope := Candidate{LeaseName: "matrix", LeaseNamespace: "example"}

	retired = NewPostgresGate(dsn, retiredCfg)
	first := scope
	first.Identity = "candidate-a"
	require.NoError(t, retired.Acquire(t.Context(), first))

	promoted = NewPostgresGate(dsn, promotedCfg)
	second := scope
	second.Identity = "candidate-b"
	require.NoError(t, promoted.Acquire(t.Context(), second))
	return retired, promoted, dsn
}

// pausedCandidate never refreshes its confirmation, which is what a process
// suspended past its lease looks like from inside.
func pausedCandidate() config.HA { return testHAConfig(100*time.Millisecond, time.Hour) }

// healthyCandidate keeps its confirmation fresh.
func healthyCandidate() config.HA { return testHAConfig(time.Hour, 20*time.Millisecond) }

// TestPausedOwnerIsRefusedAtEveryBoundary is the acceptance case for #349: a
// leader paused beyond its lease expiry, resumed after a successor took over,
// with full connectivity to every dependency and no knowledge that it lost the
// Lease. It must be unable to commit a stale authoritative effect anywhere.
func TestPausedOwnerIsRefusedAtEveryBoundary(t *testing.T) {
	// A long freshness window would let a paused owner act on a stale
	// confirmation, so the window is what the test must expire.
	retired, promoted, dsn := takeover(t, pausedCandidate(), healthyCandidate())

	// The pause is modeled by time passing without a heartbeat.
	require.Eventually(t, func() bool { return retired.Valid() != nil }, time.Second, 10*time.Millisecond)

	t.Run("database", func(t *testing.T) {
		writes := activationConn(t, dsn, *retired.Activation())
		_, err := writes.ExecContext(t.Context(), "CREATE TABLE IF NOT EXISTS stale_effect (value TEXT)")
		require.Error(t, err, "a resumed owner must not commit")
	})

	t.Run("broker", func(t *testing.T) {
		protected := jetstream.Protected(nil, retired)
		_, err := protected.PublishMsg(&nats.Msg{Subject: "owned", Data: []byte("stale")})
		require.Error(t, err, "a resumed owner must not publish")
	})

	t.Run("admission", func(t *testing.T) {
		require.False(t, Admit(func() bool { return true }, retired)(),
			"a resumed owner must not admit application requests")
	})

	t.Run("dispatch", func(t *testing.T) {
		guarded := basepkg.GuardFederationClient(nil, retired)
		_, err := guarded.SendTransaction(t.Context(), gomatrixserverlib.Transaction{})
		require.Error(t, err, "a resumed owner must not start a new outbound transaction")
	})

	t.Run("successor is unaffected", func(t *testing.T) {
		require.NoError(t, promoted.Valid())
		writes := activationConn(t, dsn, *promoted.Activation())
		_, err := writes.ExecContext(t.Context(), "CREATE TABLE IF NOT EXISTS promoted_effect (value TEXT)")
		require.NoError(t, err)
	})
}

// TestOwnerPartitionedFromKubernetesCannotOverlap covers the asymmetric
// partition: the leader cannot reach the Kubernetes API, so its election loop
// learns nothing, but PostgreSQL and NATS remain reachable.
func TestOwnerPartitionedFromKubernetesCannotOverlap(t *testing.T) {
	retired, promoted, dsn := takeover(t, healthyCandidate(), healthyCandidate())

	// Ownership is never confirmed by the Lease here, only by the database the
	// partitioned leader can still reach, which is what retires it.
	require.Eventually(t, func() bool { return errors.Is(retired.Valid(), ErrOwnershipRetired) }, 2*time.Second, 20*time.Millisecond,
		"a leader that keeps data-plane access must still discover it was replaced")

	writes := activationConn(t, dsn, *retired.Activation())
	_, err := writes.ExecContext(t.Context(), "SELECT 1")
	require.Error(t, err, "the partitioned leader must not overlap with the successor")
	require.NoError(t, promoted.Valid())
}

// TestUncertainFencingBlocksTakeover covers the fail-closed rule: when a
// candidate cannot prove that every retired backend is gone, it must stay
// unready and name the boundary rather than activate anyway.
func TestUncertainFencingBlocksTakeover(t *testing.T) {
	cfg := testHAConfig(time.Hour, time.Hour)
	cfg.LeaseName, cfg.LeaseNamespace = "matrix", "example"
	dsn, cleanup := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	t.Cleanup(cleanup)

	first := NewPostgresGate(dsn, cfg)
	require.NoError(t, first.Acquire(t.Context(), Candidate{Identity: "candidate-a", LeaseName: "matrix", LeaseNamespace: "example"}))
	held := activationConn(t, dsn, *first.Activation())
	require.NoError(t, held.PingContext(t.Context()))

	conn, err := pgx.Connect(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	successor := Activation{Generation: first.Activation().Generation + 1, Identity: "candidate-b", Scope: "example/matrix"}

	horizon, err := fenceHorizon(t.Context(), conn)
	require.NoError(t, err)

	// The sweep cannot complete within its budget, so it must report the
	// remaining backends instead of reporting success.
	expired, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	err = successorGate(dsn, cfg).sweep(expired, conn, successor, horizon)
	require.Error(t, err)
	require.ErrorContains(t, err, "retired activation", "the blocking reason must be observable")
}

func successorGate(dsn string, cfg config.HA) *PostgresGate { return NewPostgresGate(dsn, cfg) }

// TestDispatchedTransactionsAreNotRecalled documents the external delivery
// guarantee: a transaction already in flight completes, and only newly
// admitted dispatch is refused. Delivery is at least once, never exactly once.
func TestDispatchedTransactionsAreNotRecalled(t *testing.T) {
	cfg := testHAConfig(time.Hour, time.Hour)
	cfg.LeaseName, cfg.LeaseNamespace = "matrix", "example"
	dsn, cleanup := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	t.Cleanup(cleanup)
	gate := NewPostgresGate(dsn, cfg)
	require.NoError(t, gate.Acquire(t.Context(), Candidate{Identity: "candidate-a", LeaseName: "matrix", LeaseNamespace: "example"}))

	dispatched := make(chan struct{})
	release := make(chan struct{})
	inner := &blockingFederationClient{dispatched: dispatched, release: release}
	guarded := basepkg.GuardFederationClient(inner, gate)

	inFlight := make(chan error, 1)
	go func() {
		_, err := guarded.SendTransaction(t.Context(), gomatrixserverlib.Transaction{})
		inFlight <- err
	}()
	<-dispatched

	// Ownership is lost while the request is on the wire.
	gate.Retire()
	_, err := guarded.SendTransaction(t.Context(), gomatrixserverlib.Transaction{})
	require.ErrorIs(t, err, ErrOwnershipRetired, "new dispatch must be refused")

	close(release)
	require.NoError(t, <-inFlight, "a transaction already dispatched is not recalled")
}

type blockingFederationClient struct {
	fclient.FederationClient
	dispatched chan struct{}
	release    chan struct{}
}

func (c *blockingFederationClient) SendTransaction(context.Context, gomatrixserverlib.Transaction) (fclient.RespSend, error) {
	close(c.dispatched)
	<-c.release
	return fclient.RespSend{}, nil
}

// stuckApplication never returns from Run, modeling a constructor or worker
// that does not observe cancellation.
type stuckApplication struct {
	stopped chan struct{}
	once    sync.Once
}

func (a *stuckApplication) Run(context.Context) error { <-a.stopped; return nil }
func (a *stuckApplication) Stop()                     { a.once.Do(func() {}) }

// TestStuckApplicationForcesTermination exercises the shutdown budget. An
// application that will not stop must surface ErrShutdownTimeout, which the
// binary turns into a nonzero exit rather than returning to standby.
func TestStuckApplicationForcesTermination(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		leader, lose := context.WithCancelCause(ctx)
		defer lose(nil)
		acquired := make(chan context.Context, 1)
		elector := electionFunc(func(ctx context.Context) {
			acquired <- leader
			select {
			case <-ctx.Done():
			case <-leader.Done():
			}
		})
		app := &stuckApplication{stopped: make(chan struct{})}
		defer close(app.stopped)

		done := make(chan error, 1)
		go func() {
			done <- control(ctx, elector, acquired, nil, Candidate{}, gateFunc(func(context.Context, Candidate) error { return nil }), app, time.Second)
		}()
		synctest.Wait()
		lose(ErrLeadershipLost)

		err := <-done
		require.ErrorIs(t, err, ErrLeadershipLost)
		require.ErrorIs(t, err, lifecycle.ErrShutdownTimeout, "a stuck application must force process termination")
	})
}
