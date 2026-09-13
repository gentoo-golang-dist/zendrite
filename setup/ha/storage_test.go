package ha

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/test"
)

func TestStorageCompatibilityAndMigrationSerialization(t *testing.T) {
	dsn, cleanup := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer cleanup()
	cfg := &config.Zendrite{}
	cfg.Global.HA = config.HA{Enabled: true, Compatibility: "same-binary-and-config"}
	cfg.Global.DatabaseOptions.ConnectionString = config.DataSource(dsn)
	release, err := PrepareStorage(t.Context(), cfg)
	require.NoError(t, err)
	defer release()
	// Even another authorized startup cannot migrate concurrently.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = PrepareStorage(ctx, cfg)
	require.ErrorContains(t, err, "migration lock")
	release()
	again, err := PrepareStorage(t.Context(), cfg)
	require.NoError(t, err)
	again()
	unsafe := *cfg
	unsafe.Global.DatabaseOptions.ConnectionString += " synchronous_commit=off"
	_, err = PrepareStorage(t.Context(), &unsafe)
	require.ErrorContains(t, err, "synchronous_commit")
	cfg.Global.HA.Compatibility = "different-binary"
	_, err = PrepareStorage(t.Context(), cfg)
	require.ErrorContains(t, err, "configuration or executable differs")
}

func TestLeaseCompatibility(t *testing.T) {
	for _, fingerprint := range []string{"expected", "different", ""} {
		t.Run("fingerprint="+fingerprint, func(t *testing.T) {
			client := fake.NewClientset(&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "matrix", Namespace: "test", Annotations: map[string]string{compatibilityAnnotation: fingerprint}}})
			failures := make(chan error, 1)
			lock := &permissionLock{Interface: &resourcelock.LeaseLock{LeaseMeta: metav1.ObjectMeta{Name: "matrix", Namespace: "test"}, Client: client.CoordinationV1()}, failures: failures, leases: client.CoordinationV1().Leases("test"), leaseName: "matrix", compatibility: "expected"}
			_, _, err := lock.Get(t.Context())
			if fingerprint == "expected" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "compatibility mismatch")
				require.Len(t, failures, 1)
			}
		})
	}
}
