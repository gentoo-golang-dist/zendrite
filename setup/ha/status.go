package ha

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/setup/lifecycle"
)

// Phase describes what a candidate is doing. It is reported separately from
// readiness so that an operator can tell a healthy standby apart from a
// candidate that is stuck establishing ownership or recovering.
type Phase string

const (
	PhaseStarting   Phase = "starting"
	PhaseStandby    Phase = "standby"
	PhaseFencing    Phase = "fencing"
	PhaseRecovering Phase = "recovering"
	PhaseActive     Phase = "active"
	PhaseDraining   Phase = "draining"
	PhaseStopped    Phase = "stopped"
)

// Status is the candidate's probe and observability surface.
//
// It exists because a standby never binds the Matrix listeners: without a
// separate surface a standby would look dead to Kubernetes and be restarted,
// and its metrics would be unscrapable.
type Status struct {
	mu         sync.Mutex
	phase      Phase
	since      time.Time
	reason     string
	boundary   Boundary
	server     StateReporter
	acquiredAt time.Time
	lostAt     time.Time
}

// StateReporter is the activation's own view of itself, which is more
// authoritative than anything the election loop can infer.
type StateReporter interface{ State() lifecycle.State }

// NewStatus returns a status surface for a candidate that has not yet begun
// electing. It reports live but never ready.
func NewStatus() *Status {
	return &Status{phase: PhaseStarting, since: time.Now()}
}

// Track follows an activation's own lifecycle, so recovery, activity, and
// draining are reported from the homeserver rather than guessed by the caller.
func (s *Status) Track(server StateReporter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.server = server
}

// Observe records a phase transition and clears any previous blocking reason.
func (s *Status) Observe(phase Phase, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enter(phase)
	s.reason = reason
	s.boundary = ""
}

// enter records a transition once, wherever it is first observed. Callers hold
// the lock. Transitions arrive both from the election loop and from the
// activation's own state, so accounting lives here rather than at each caller.
func (s *Status) enter(phase Phase) {
	if s.phase == phase {
		return
	}
	previous := s.phase
	s.phase = phase
	s.since = time.Now()
	observePhase(phase)
	switch phase {
	case PhaseFencing:
		s.acquiredAt = s.since
		leadershipTransitions.WithLabelValues("acquired").Inc()
	case PhaseActive:
		if !s.acquiredAt.IsZero() {
			recoveryDuration.Set(s.since.Sub(s.acquiredAt).Seconds())
		}
	case PhaseDraining:
		s.lostAt = s.since
		if previous == PhaseActive || previous == PhaseRecovering || previous == PhaseFencing {
			leadershipTransitions.WithLabelValues("lost").Inc()
		}
	case PhaseStopped:
		if !s.lostAt.IsZero() {
			shutdownDuration.Set(s.since.Sub(s.lostAt).Seconds())
		}
	case PhaseStarting, PhaseStandby, PhaseRecovering:
	}
}

// Fail records why a candidate cannot proceed, naming the blocking boundary
// when the failure came from ownership establishment.
func (s *Status) Fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reason = err.Error()
	var blocked *BlockedError
	if errors.As(err, &blocked) {
		s.boundary = blocked.Boundary
		fencingFailures.WithLabelValues(string(blocked.Boundary)).Inc()
	}
}

// Phase reports the current phase, preferring the activation's own state once
// one exists so that a stopped or draining homeserver is never reported active.
func (s *Status) Phase() Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enter(s.phase.merge(s.server))
	return s.phase
}

func (p Phase) merge(server StateReporter) Phase {
	if server == nil {
		return p
	}
	switch server.State() {
	case lifecycle.Recovering:
		return PhaseRecovering
	case lifecycle.Active:
		return PhaseActive
	case lifecycle.Draining:
		return PhaseDraining
	case lifecycle.Stopped:
		return PhaseStopped
	case lifecycle.Standby:
		return p
	}
	return p
}

// Ready reports whether this candidate should receive Matrix traffic.
func (s *Status) Ready() bool { return s.Phase() == PhaseActive }

// Handler serves the probe and status endpoints.
//
// Liveness and readiness are deliberately different: a standby is live, so a
// liveness probe must not restart it, and unready, so a Service must not route
// to it. An elected candidate stays unready until fencing and recovery finish.
func (s *Status) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_zendrite/monitor/up", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /_zendrite/monitor/health", func(w http.ResponseWriter, r *http.Request) {
		if !s.Ready() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			s.write(w)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /_zendrite/monitor/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		s.write(w)
	})
	return mux
}

type statusBody struct {
	Phase    Phase    `json:"phase"`
	Leader   bool     `json:"leader"`
	Ready    bool     `json:"ready"`
	Since    string   `json:"since"`
	Reason   string   `json:"reason,omitempty"`
	Boundary Boundary `json:"blocked_boundary,omitempty"`
}

func (s *Status) write(w http.ResponseWriter) {
	s.mu.Lock()
	s.enter(s.phase.merge(s.server))
	body := statusBody{
		Phase:    s.phase,
		Since:    s.since.UTC().Format(time.RFC3339),
		Reason:   s.reason,
		Boundary: s.boundary,
	}
	s.mu.Unlock()
	body.Ready = body.Phase == PhaseActive
	body.Leader = body.Phase != PhaseStarting && body.Phase != PhaseStandby
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logrus.WithError(err).Debug("Failed to write HA status response")
	}
}
