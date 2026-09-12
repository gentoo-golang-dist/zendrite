package ha

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/lifecycle"
	"codefloe.com/pat-s/zendrite/setup/process"
)

type gateFunc func(context.Context, Candidate) error

func (f gateFunc) Acquire(ctx context.Context, c Candidate) error { return f(ctx, c) }

type electionFunc func(context.Context)

func (f electionFunc) Run(ctx context.Context) { f(ctx) }

func TestControl(t *testing.T) {
	for _, scenario := range []string{"standby cancellation", "missing gate", "gate failure", "loss during fencing", "loss during recovery", "active loss", "blocked gate", "startup failure"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				leader, lose := context.WithCancelCause(ctx)
				defer lose(nil)
				acquired := make(chan context.Context, 1)
				elector := electionFunc(func(ctx context.Context) {
					if scenario != "standby cancellation" {
						acquired <- leader
					}
					select {
					case <-ctx.Done():
					case <-leader.Done():
					}
				})
				var calls atomic.Int32
				recovery := make(chan struct{})
				server := lifecycle.New(func(p *process.ProcessContext) error {
					calls.Add(1)
					if scenario == "loss during recovery" {
						<-recovery
					}
					if scenario == "startup failure" {
						return errors.New("startup failed")
					}
					return nil
				}, time.Second)
				gateReturned := make(chan struct{})
				unblockGate := make(chan struct{})
				var gate OwnershipGate = gateFunc(func(ctx context.Context, _ Candidate) error {
					defer close(gateReturned)
					switch scenario {
					case "gate failure":
						return errors.New("fence uncertain")
					case "loss during fencing":
						<-ctx.Done()
						return nil
					case "blocked gate":
						<-unblockGate
					}
					return nil
				})
				if scenario == "missing gate" {
					gate = nil
				}
				done := make(chan error, 1)
				go func() { done <- control(ctx, elector, acquired, nil, Candidate{}, gate, server, 2*time.Second) }()
				synctest.Wait()
				switch scenario {
				case "standby cancellation":
					if calls.Load() != 0 {
						t.Fatal("standby initialized")
					}
					cancel(nil)
				case "loss during fencing", "blocked gate":
					lose(ErrLeadershipLost)
				case "loss during recovery":
					if server.Ready() {
						t.Fatal("recovering server ready")
					}
					lose(ErrLeadershipLost)
					synctest.Wait()
					close(recovery)
				case "active loss":
					if !server.Ready() {
						t.Fatal("fenced leader not ready")
					}
					lose(ErrLeadershipLost)
				}
				err := <-done
				switch scenario {
				case "standby cancellation":
					if err != nil {
						t.Fatal(err)
					}
				case "missing gate":
					if !errors.Is(err, ErrFencingUnavailable) {
						t.Fatal(err)
					}
				case "blocked gate":
					if !errors.Is(err, lifecycle.ErrShutdownTimeout) {
						t.Fatal(err)
					}
					close(unblockGate)
					<-gateReturned
				default:
					if err == nil {
						t.Fatal("expected failure")
					}
				}
				if server.Ready() {
					t.Fatal("stopped leader published readiness")
				}
				if scenario == "missing gate" || scenario == "gate failure" || scenario == "loss during fencing" || scenario == "blocked gate" {
					if calls.Load() != 0 {
						t.Fatal("initialized without fenced ownership")
					}
				}
			})
		})
	}
}

func TestDisabledDoesNotUseKubernetesOrGate(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	server := lifecycle.New(func(p *process.ProcessContext) error { cancel(nil); return nil }, time.Second)
	gate := gateFunc(func(context.Context, Candidate) error { t.Error("standalone called fencing"); return nil })
	// Invalid HA-only options are ignored, and no service account is required.
	if err := Run(ctx, config.HA{Backend: "invalid"}, server, gate, time.Second); err != nil {
		t.Fatal(err)
	}
}
