package config

import (
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

func TestHAConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*HA)
		invalid bool
	}{
		{"valid", func(*HA) {}, false},
		{"disabled", func(c *HA) { *c = HA{Backend: "unknown"} }, false},
		{"backend", func(c *HA) { c.Backend = "postgres" }, true},
		{"identity", func(c *HA) { c.CandidateID = " " }, true},
		{"namespace", func(c *HA) { c.LeaseNamespace = "a.b" }, true},
		{"lease", func(c *HA) { c.LeaseName = "" }, true},
		{"deadline", func(c *HA) { c.RenewDeadline = c.LeaseDuration }, true},
		{"jitter", func(c *HA) { c.RenewDeadline = 6 * time.Second }, true},
		{"retry", func(c *HA) { c.RetryPeriod = 0 }, true},
		{"subsecond", func(c *HA) {
			c.LeaseDuration = time.Second / 2
			c.RenewDeadline = time.Millisecond * 100
			c.RetryPeriod = time.Millisecond
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := HA{}
			c.Defaults()
			c.Enabled = true
			c.CandidateID = "pod-uid"
			c.LeaseName = "matrix-example-org"
			c.LeaseNamespace = "matrix"
			tc.change(&c)
			errs := &ConfigErrors{}
			c.Verify(errs)
			if (len(*errs) > 0) != tc.invalid {
				t.Fatalf("unexpected validation: %v", errs)
			}
		})
	}
}

func TestHAYAMLDefaults(t *testing.T) {
	var c Global
	c.Defaults(DefaultOpts{})
	if err := yaml.Unmarshal([]byte("ha:\n  enabled: true\n  candidate_id: pod-uid\n  lease_name: matrix\n  lease_namespace: matrix\n  retry_period: 4s\n"), &c); err != nil {
		t.Fatal(err)
	}
	if c.HA.Backend != "kubernetes" || c.HA.LeaseDuration != 30*time.Second || c.HA.RetryPeriod != 4*time.Second {
		t.Fatalf("unexpected defaults: %+v", c.HA)
	}
}
