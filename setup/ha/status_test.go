package ha

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/lifecycle"
	"codefloe.com/pat-s/zendrite/setup/process"
)

func TestStatusReportsStandbyAsLiveButNotReady(t *testing.T) {
	status := NewStatus()
	require.Equal(t, PhaseStarting, status.Phase())
	status.Observe(PhaseStandby, "")

	require.Equal(t, http.StatusOK, probe(t, status, "/_zendrite/monitor/up"),
		"a healthy standby must be live so probes do not restart it")
	require.Equal(t, http.StatusServiceUnavailable, probe(t, status, "/_zendrite/monitor/health"),
		"a standby must not be routed Matrix traffic")
}

func TestStatusReportsReadyOnlyWhenActive(t *testing.T) {
	status := NewStatus()
	for _, phase := range []Phase{PhaseFencing, PhaseRecovering, PhaseDraining, PhaseStopped} {
		status.Observe(phase, "")
		require.Equal(t, http.StatusServiceUnavailable, probe(t, status, "/_zendrite/monitor/health"),
			"phase %q must not be ready", phase)
	}
	status.Observe(PhaseActive, "")
	require.Equal(t, http.StatusOK, probe(t, status, "/_zendrite/monitor/health"))
}

func TestStatusExposesTheBlockingReason(t *testing.T) {
	status := NewStatus()
	status.Observe(PhaseFencing, "")
	status.Fail(&BlockedError{Boundary: BoundaryPostgres, Err: errors.New("2 backend(s) of a retired activation are still connected")})

	recorder := httptest.NewRecorder()
	status.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/_zendrite/monitor/status", nil))
	require.Equal(t, http.StatusOK, recorder.Code)

	var body struct {
		Phase    string `json:"phase"`
		Leader   bool   `json:"leader"`
		Reason   string `json:"reason"`
		Boundary string `json:"blocked_boundary"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, string(PhaseFencing), body.Phase)
	require.Equal(t, string(BoundaryPostgres), body.Boundary)
	require.Contains(t, body.Reason, "retired activation")
}

func TestStatusFollowsTheActivationLifecycle(t *testing.T) {
	server := lifecycle.New(func(*process.ProcessContext) error { return nil }, time.Second)
	status := NewStatus()
	status.Track(server)
	status.Observe(PhaseRecovering, "")

	server.Stop()
	require.Equal(t, PhaseStopped, status.Phase(), "the status must follow the activation rather than report a stale phase")
}

func probe(t *testing.T, status *Status, path string) int {
	t.Helper()
	recorder := httptest.NewRecorder()
	status.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder.Code
}

// TestStatusListenerServesStandbyCandidates covers the reason this listener
// exists: a standby never binds the Matrix listeners, so without it Kubernetes
// would see a dead pod and restart a perfectly healthy candidate.
func TestStatusListenerServesStandbyCandidates(t *testing.T) {
	status := NewStatus()
	status.Observe(PhaseStandby, "")
	cfg := config.HA{StatusAddress: "127.0.0.1:0"}

	listener, serve, err := listenStatus(cfg, status, &config.Metrics{Enabled: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go serve()

	base := "http://" + listener.Addr().String()
	require.Equal(t, http.StatusOK, get(t, base+"/_zendrite/monitor/up"))
	require.Equal(t, http.StatusServiceUnavailable, get(t, base+"/_zendrite/monitor/health"))
	require.Equal(t, http.StatusOK, get(t, base+"/metrics"),
		"standby metrics must be scrapable, or a candidate is invisible until it is promoted")
}

func get(t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestStatusRecordsLeadershipAndRecoveryMetrics(t *testing.T) {
	resetHAMetrics()
	status := NewStatus()
	status.Observe(PhaseStandby, "")
	require.Equal(t, 0.0, counterValue(t, leadershipTransitions, "acquired"))

	status.Observe(PhaseFencing, "")
	require.Equal(t, 1.0, counterValue(t, leadershipTransitions, "acquired"))
	require.Equal(t, 1.0, gaugeVecValue(t, currentPhase, string(PhaseFencing)))
	require.Equal(t, 0.0, gaugeVecValue(t, currentPhase, string(PhaseStandby)),
		"exactly one phase series must be set so alerts can select on a phase")

	status.Observe(PhaseRecovering, "")
	status.Observe(PhaseActive, "")
	require.GreaterOrEqual(t, gaugeValue(t, recoveryDuration), 0.0, "recovery time must be measured from acquisition")

	status.Observe(PhaseDraining, "")
	require.Equal(t, 1.0, counterValue(t, leadershipTransitions, "lost"))
	status.Observe(PhaseStopped, "")
	require.GreaterOrEqual(t, gaugeValue(t, shutdownDuration), 0.0, "shutdown time must be measured from ownership loss")
}

func TestStatusCountsFencingFailuresByBoundary(t *testing.T) {
	resetHAMetrics()
	status := NewStatus()
	status.Fail(&BlockedError{Boundary: BoundaryJetStream, Err: errors.New("broker unreachable")})
	require.Equal(t, 1.0, counterValue(t, fencingFailures, string(BoundaryJetStream)))
	require.Equal(t, 0.0, counterValue(t, fencingFailures, string(BoundaryPostgres)))
}

func TestHAMetricLabelsAreBounded(t *testing.T) {
	resetHAMetrics()
	status := NewStatus()
	for _, phase := range allPhases {
		status.Observe(phase, "")
	}
	require.Len(t, collect(t, currentPhase), len(allPhases),
		"phase labels must stay bounded; candidate identity and generation must never become labels")
	require.Len(t, collect(t, fencingFailures), len(requiredBoundaries))
	require.Len(t, collect(t, leadershipTransitions), 2)
}

func TestRenewalFailuresAreCounted(t *testing.T) {
	// Counters cannot be reset, so this asserts on the delta.
	before := counterValue2(t, renewalFailures)
	lock := &permissionLock{Interface: failingLock{}, failures: make(chan error, 1)}
	require.Error(t, lock.Update(t.Context(), resourcelock.LeaderElectionRecord{}))
	require.Equal(t, before+1, counterValue2(t, renewalFailures))
}

type failingLock struct{ resourcelock.Interface }

func (failingLock) Update(context.Context, resourcelock.LeaderElectionRecord) error {
	return errors.New("lease renewal failed")
}
