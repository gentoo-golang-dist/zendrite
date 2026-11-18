// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/internal/retention"
	"codefloe.com/pat-s/zendrite/setup/config"
)

// retentionPolicyJSON is a retention policy as the MSC1763 configuration
// endpoint reports it, in milliseconds.
type retentionPolicyJSON struct {
	MinLifetime int64 `json:"min_lifetime,omitempty"`
	MaxLifetime int64 `json:"max_lifetime,omitempty"`
}

// lifetimeBoundsJSON is the range a room's lifetime is brought into.
type lifetimeBoundsJSON struct {
	Min int64 `json:"min,omitempty"`
	Max int64 `json:"max,omitempty"`
}

type retentionLimitsJSON struct {
	MaxLifetime *lifetimeBoundsJSON `json:"max_lifetime,omitempty"`
}

type retentionConfigurationResponse struct {
	Policies map[string]retentionPolicyJSON `json:"policies"`
	Limits   retentionLimitsJSON            `json:"limits"`
}

// GetRetentionConfiguration implements MSC1763's
// GET /_matrix/client/unstable/org.matrix.msc1763/retention/configuration:
// the server's default policy, under "*", and the bounds of max_lifetime,
// as in Synapse.
func GetRetentionConfiguration(cfg *config.Retention) util.JSONResponse {
	res := retentionConfigurationResponse{Policies: map[string]retentionPolicyJSON{}}
	def := retentionPolicyJSON{
		MinLifetime: cfg.DefaultPolicy.MinLifetime.Milliseconds(),
		MaxLifetime: cfg.DefaultPolicy.MaxLifetime.Milliseconds(),
	}
	if def != (retentionPolicyJSON{}) {
		res.Policies["*"] = def
	}
	bounds := lifetimeBoundsJSON{
		Min: cfg.AllowedLifetimeMin.Milliseconds(),
		Max: cfg.AllowedLifetimeMax.Milliseconds(),
	}
	if bounds != (lifetimeBoundsJSON{}) {
		res.Limits.MaxLifetime = &bounds
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: res}
}

// validateRetentionEvent rejects an m.room.retention event a local user
// sends whose content is malformed, as Synapse does whether or not
// retention policies are enabled.
func validateRetentionEvent(eventType string, stateKey *string, content []byte) *util.JSONResponse {
	if eventType != retention.EventType {
		return nil
	}
	if stateKey == nil {
		return &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("m.room.retention must be a state event"),
		}
	}
	if err := retention.ValidateContent(content); err != nil {
		return &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON(err.Error()),
		}
	}
	return nil
}
