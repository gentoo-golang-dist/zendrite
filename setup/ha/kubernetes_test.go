package ha

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/lifecycle"
	"codefloe.com/pat-s/zendrite/setup/process"
)

func electionConfig() config.HA {
	c := config.HA{}
	c.Defaults()
	c.Enabled = true
	c.CandidateID = "pod"
	c.LeaseName = "homeserver"
	c.LeaseNamespace = "matrix"
	return c
}

// leaseClient adds API-server resourceVersion conflict semantics to the fake.
// Reactors run under the fake's mutex, so the compare and update are atomic.
func leaseClient() *fake.Clientset {
	client := fake.NewClientset()
	var revision int
	client.PrependReactor("update", "leases", func(action ktesting.Action) (bool, runtime.Object, error) {
		updateAction, ok := action.(ktesting.UpdateAction)
		if !ok {
			return true, nil, fmt.Errorf("expected update action, got %T", action)
		}
		update, ok := updateAction.GetObject().(*coordinationv1.Lease)
		if !ok {
			return true, nil, fmt.Errorf("expected Lease, got %T", updateAction.GetObject())
		}
		old, err := client.Tracker().Get(action.GetResource(), action.GetNamespace(), update.Name)
		if err != nil {
			return true, nil, err
		}
		oldLease, ok := old.(*coordinationv1.Lease)
		if !ok {
			return true, nil, fmt.Errorf("expected stored Lease, got %T", old)
		}
		if oldLease.ResourceVersion != update.ResourceVersion {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, update.Name, errors.New("resourceVersion changed"))
		}
		revision++
		updated := update.DeepCopy()
		updated.ResourceVersion = strconv.Itoa(revision)
		err = client.Tracker().Update(action.GetResource(), updated, action.GetNamespace())
		return true, updated, err
	})
	return client
}

func TestKubernetesContention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := leaseClient()
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		var starts atomic.Int32
		var fences atomic.Int32
		identities := make(chan string, 2)
		gate := gateFunc(func(_ context.Context, c Candidate) error {
			fences.Add(1)
			identities <- c.Identity
			return nil
		})
		done := make(chan error, 2)
		servers := make([]*lifecycle.Homeserver, 2)
		for i := range servers {
			server := lifecycle.New(func(*process.ProcessContext) error { starts.Add(1); return nil }, time.Second)
			servers[i] = server
			go func() {
				done <- runKubernetes(ctx, electionConfig(), client.CoordinationV1(), server, gate, time.Second)
			}()
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if starts.Load() != 1 || fences.Load() != 1 {
			t.Fatalf("starts=%d fences=%d", starts.Load(), fences.Load())
		}
		ready := 0
		for _, server := range servers {
			if server.Ready() {
				ready++
			}
		}
		if ready != 1 {
			t.Fatalf("ready candidates=%d", ready)
		}
		identity := <-identities
		lease, err := client.CoordinationV1().Leases("matrix").Get(ctx, "homeserver", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if identity == "pod" || *lease.Spec.HolderIdentity != identity {
			t.Fatalf("unexpected identity %q", identity)
		}
		cancel(nil)
		for range servers {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		// Cancellation must not clear the Lease while protected work might be draining.
		lease, err = client.CoordinationV1().Leases("matrix").Get(t.Context(), "homeserver", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if *lease.Spec.HolderIdentity != identity {
			t.Fatal("leadership released on cancellation")
		}
	})
}

func TestKubernetesFailures(t *testing.T) {
	for _, scenario := range []string{"get denied", "create denied", "update denied", "API outage", "renewal failure"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := leaseClient()
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				var unavailable atomic.Bool
				client.PrependReactor("*", "leases", func(action ktesting.Action) (bool, runtime.Object, error) {
					if scenario == action.GetVerb()+" denied" {
						return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "homeserver", errors.New("RBAC denied"))
					}
					if scenario == "API outage" || unavailable.Load() {
						return true, nil, fmt.Errorf("API unavailable")
					}
					return false, nil, nil
				})
				var starts atomic.Int32
				server := lifecycle.New(func(*process.ProcessContext) error { starts.Add(1); return nil }, time.Second)
				done := make(chan error, 1)
				go func() {
					done <- runKubernetes(ctx, electionConfig(), client.CoordinationV1(), server, gateFunc(func(context.Context, Candidate) error { return nil }), time.Second)
				}()
				synctest.Wait()
				if scenario == "renewal failure" {
					if !server.Ready() {
						t.Fatal("leader did not activate")
					}
					unavailable.Store(true)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if scenario == "API outage" {
					if starts.Load() != 0 {
						t.Fatal("API outage allowed startup")
					}
					select {
					case err := <-done:
						t.Fatalf("outage killed standby: %v", err)
					default:
					}
					cancel(nil)
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				} else {
					err := <-done
					if err == nil {
						t.Fatal("expected failure")
					}
					if scenario == "renewal failure" && !errors.Is(err, ErrLeadershipLost) {
						t.Fatal(err)
					}
				}
				if server.Ready() {
					t.Fatal("failed candidate is ready")
				}
			})
		})
	}
}
