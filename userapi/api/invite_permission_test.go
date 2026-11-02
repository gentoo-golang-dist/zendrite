// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package api

import (
	"context"
	"encoding/json"
	"testing"
)

type accountDataFixture map[string]json.RawMessage

func (f accountDataFixture) QueryAccountData(_ context.Context, req *QueryAccountDataRequest, res *QueryAccountDataResponse) error {
	res.GlobalAccountData = map[string]json.RawMessage{}
	if data, ok := f[req.DataType]; ok {
		res.GlobalAccountData[req.DataType] = data
	}
	return nil
}

func TestInvitesBlocked(t *testing.T) {
	for _, tc := range []struct {
		name             string
		stable, unstable string
		want             bool
	}{
		{name: "no account data"},
		{name: "stable block", stable: `{"default_action":"block"}`, want: true},
		{name: "unstable block", unstable: `{"default_action":"block"}`, want: true},
		{name: "stable takes precedence", stable: `{}`, unstable: `{"default_action":"block"}`},
		{name: "other action", stable: `{"default_action":"allow"}`},
		{name: "action not a string", stable: `{"default_action":true}`},
		{name: "action case sensitive", stable: `{"default_action":"BLOCK"}`},
		{name: "content not an object", stable: `"block"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := accountDataFixture{}
			if tc.stable != "" {
				data[InvitePermissionConfigType] = json.RawMessage(tc.stable)
			}
			if tc.unstable != "" {
				data[UnstableInvitePermissionConfigType] = json.RawMessage(tc.unstable)
			}
			got, err := InvitesBlocked(context.Background(), data, "@bob:test")
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	if got, err := InvitesBlocked(context.Background(), nil, "@bob:test"); err != nil || got {
		t.Fatalf("nil querier: got %v, %v", got, err)
	}
}
