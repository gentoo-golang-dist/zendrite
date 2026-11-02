// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package api

import (
	"context"
	"encoding/json"
)

// Account data types that configure invite blocking (MSC4380, spec v1.18).
const (
	InvitePermissionConfigType         = "m.invite_permission_config"
	UnstableInvitePermissionConfigType = "org.matrix.msc4380.invite_permission_config"
)

// AccountDataQuerier reads a user's account data.
type AccountDataQuerier interface {
	QueryAccountData(ctx context.Context, req *QueryAccountDataRequest, res *QueryAccountDataResponse) error
}

// InvitesBlocked reports whether the local user blocks every room invite: the
// m.invite_permission_config account data sets default_action to "block".
// The stable type takes precedence; the unstable one is read when the stable
// one is absent.
func InvitesBlocked(ctx context.Context, q AccountDataQuerier, userID string) (bool, error) {
	if q == nil {
		return false, nil
	}
	for _, dataType := range []string{InvitePermissionConfigType, UnstableInvitePermissionConfigType} {
		var res QueryAccountDataResponse
		if err := q.QueryAccountData(ctx, &QueryAccountDataRequest{UserID: userID, DataType: dataType}, &res); err != nil {
			return false, err
		}
		data, ok := res.GlobalAccountData[dataType]
		if !ok || len(data) == 0 {
			continue
		}
		var config struct {
			DefaultAction any `json:"default_action"`
		}
		// Content that is not an object configures nothing.
		if err := json.Unmarshal(data, &config); err != nil {
			return false, nil
		}
		return config.DefaultAction == "block", nil
	}
	return false, nil
}
