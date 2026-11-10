// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

type failingQuerier struct{}

func (failingQuerier) QueryAccountData(context.Context, *QueryAccountDataRequest, *QueryAccountDataResponse) error {
	return errors.New("boom")
}

// TestInviteConfigMSC4380 covers m.invite_permission_config (MSC4380): it
// blocks every invite or none, and an empty one falls through to MSC4155.
func TestInviteConfigMSC4380(t *testing.T) {
	blockAll := `{"blocked_servers":["*"]}`
	for _, tc := range []struct {
		name                      string
		stable, unstable, msc4155 string
		want                      InviteRule
	}{
		{name: "no account data", want: InviteAllow},
		{name: "stable block", stable: `{"default_action":"block"}`, want: InviteBlock},
		{name: "unstable block", unstable: `{"default_action":"block"}`, want: InviteBlock},
		{name: "empty stable hides unstable", stable: `{}`, unstable: `{"default_action":"block"}`, want: InviteAllow},
		{name: "other action", stable: `{"default_action":"allow"}`, want: InviteAllow},
		{name: "action not a string", stable: `{"default_action":true}`, want: InviteAllow},
		{name: "action case sensitive", stable: `{"default_action":"BLOCK"}`, want: InviteAllow},
		{name: "content not an object", stable: `"block"`, want: InviteAllow},
		// Synapse: a stated MSC4380 preference wins over MSC4155.
		{name: "stable allow wins over MSC4155", stable: `{"default_action":"allow"}`, msc4155: blockAll, want: InviteAllow},
		{name: "unstable allow wins over MSC4155", unstable: `{"default_action":"allow"}`, msc4155: blockAll, want: InviteAllow},
		{name: "stable block wins over MSC4155", stable: `{"default_action":"block"}`, msc4155: `{"allowed_users":["*"]}`, want: InviteBlock},
		// An empty one states no preference.
		{name: "empty stable falls through", stable: `{}`, msc4155: blockAll, want: InviteBlock},
		{name: "empty unstable falls through", unstable: `{}`, msc4155: blockAll, want: InviteBlock},
		{name: "non-object falls through", stable: `[]`, msc4155: blockAll, want: InviteBlock},
		{name: "MSC4155 only", msc4155: blockAll, want: InviteBlock},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := accountDataFixture{}
			if tc.stable != "" {
				data[InvitePermissionConfigType] = json.RawMessage(tc.stable)
			}
			if tc.unstable != "" {
				data[UnstableInvitePermissionConfigType] = json.RawMessage(tc.unstable)
			}
			if tc.msc4155 != "" {
				data[MSC4155InvitePermissionConfigType] = json.RawMessage(tc.msc4155)
			}
			got, err := InviteRuleFor(context.Background(), data, "@bob:test", "@alice:example.org")
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	if got, err := InviteRuleFor(context.Background(), nil, "@bob:test", "@alice:test"); err != nil || got != InviteAllow {
		t.Fatalf("nil querier: got %v, %v", got, err)
	}
	if _, err := InviteRuleFor(context.Background(), failingQuerier{}, "@bob:test", "@alice:test"); err == nil {
		t.Fatal("expected the querier's error")
	}
}

func TestInviteConfigBlocksAll(t *testing.T) {
	var nilConfig *InviteConfig
	if nilConfig.BlocksAll() || nilConfig.Rule("@a:b") != InviteAllow {
		t.Fatal("a nil config must allow every invite")
	}
	config, err := LoadInviteConfig(context.Background(), accountDataFixture{
		InvitePermissionConfigType: json.RawMessage(`{"default_action":"block"}`),
	}, "@bob:test")
	if err != nil || !config.BlocksAll() {
		t.Fatalf("got %v, %v", config, err)
	}
	// MSC4155's block-everything is a rule, not BlocksAll, so allow lists
	// keep working.
	config = parseMSC4155(json.RawMessage(`{"blocked_servers":["*"]}`))
	if config.BlocksAll() {
		t.Fatal("MSC4155 must not set BlocksAll")
	}
}

// TestInviteConfigMSC4155 covers the MSC4155 lists and their precedence.
func TestInviteConfigMSC4155(t *testing.T) {
	long := `"@` + strings.Repeat("a", 260) + `:x"`
	for _, tc := range []struct {
		name    string
		content string
		inviter string
		want    InviteRule
	}{
		{name: "empty allows", content: `{}`, inviter: "@a:x.org", want: InviteAllow},
		{name: "block everyone", content: `{"blocked_servers":["*"]}`, inviter: "@a:x.org", want: InviteBlock},
		{name: "block user", content: `{"blocked_users":["@a:x.org"]}`, inviter: "@a:x.org", want: InviteBlock},
		{name: "block other user", content: `{"blocked_users":["@a:x.org"]}`, inviter: "@b:x.org", want: InviteAllow},
		{name: "ignore user", content: `{"ignored_users":["@a:x.org"]}`, inviter: "@a:x.org", want: InviteIgnore},
		{name: "ignore server", content: `{"ignored_servers":["x.org"]}`, inviter: "@a:x.org", want: InviteIgnore},
		{name: "user glob", content: `{"blocked_users":["@spam*:*"]}`, inviter: "@spammer:y.org", want: InviteBlock},
		{name: "single character glob", content: `{"blocked_users":["@?:x.org"]}`, inviter: "@a:x.org", want: InviteBlock},
		{name: "single character glob no match", content: `{"blocked_users":["@?:x.org"]}`, inviter: "@ab:x.org", want: InviteAllow},
		{name: "glob is anchored", content: `{"blocked_servers":["x.org"]}`, inviter: "@a:evilx.org", want: InviteAllow},
		{name: "glob is not a regex", content: `{"blocked_servers":["x.org"]}`, inviter: "@a:xxorg", want: InviteAllow},
		{name: "glob ignores case", content: `{"blocked_servers":["X.ORG"]}`, inviter: "@a:x.org", want: InviteBlock},
		{name: "subdomain glob", content: `{"blocked_servers":["*.x.org"]}`, inviter: "@a:m.x.org", want: InviteBlock},
		// Allow beats ignore beats block within users, and users beat servers.
		{name: "allowed user beats blocked user", content: `{"allowed_users":["@a:x.org"],"blocked_users":["*"]}`, inviter: "@a:x.org", want: InviteAllow},
		{name: "allowed user beats ignored user", content: `{"allowed_users":["@a:x.org"],"ignored_users":["*"]}`, inviter: "@a:x.org", want: InviteAllow},
		{name: "ignored user beats blocked user", content: `{"ignored_users":["*"],"blocked_users":["*"]}`, inviter: "@a:x.org", want: InviteIgnore},
		{name: "allowed user beats blocked server", content: `{"allowed_users":["@goodguy:x.org"],"blocked_servers":["x.org"]}`, inviter: "@goodguy:x.org", want: InviteAllow},
		{name: "blocked user beats allowed server", content: `{"blocked_users":["@bad:x.org"],"allowed_servers":["x.org"],"blocked_servers":["*"]}`, inviter: "@bad:x.org", want: InviteBlock},
		{name: "allowed server", content: `{"allowed_servers":["x.org"],"blocked_servers":["*"]}`, inviter: "@a:x.org", want: InviteAllow},
		{name: "not allowed server", content: `{"allowed_servers":["x.org"],"blocked_servers":["*"]}`, inviter: "@a:y.org", want: InviteBlock},
		{name: "ignored server beats blocked server", content: `{"allowed_servers":["x.org"],"ignored_servers":["y.org"],"blocked_servers":["*"]}`, inviter: "@a:y.org", want: InviteIgnore},
		{name: "allowed server beats ignored server", content: `{"allowed_servers":["*"],"ignored_servers":["*"]}`, inviter: "@a:y.org", want: InviteAllow},
		// Server globs match with or without the port.
		{name: "server glob without port", content: `{"blocked_servers":["x.org"]}`, inviter: "@a:x.org:8448", want: InviteBlock},
		{name: "server glob with port", content: `{"blocked_servers":["x.org:8448"]}`, inviter: "@a:x.org:8448", want: InviteBlock},
		{name: "IPv6 literal", content: `{"blocked_servers":["[::1]"]}`, inviter: "@a:[::1]:8448", want: InviteBlock},
		{name: "IPv6 literal without port", content: `{"blocked_servers":["[::1]"]}`, inviter: "@a:[::1]", want: InviteBlock},
		// enabled.
		{name: "disabled", content: `{"enabled":false,"blocked_servers":["*"]}`, inviter: "@a:x.org", want: InviteAllow},
		{name: "enabled", content: `{"enabled":true,"blocked_servers":["*"]}`, inviter: "@a:x.org", want: InviteBlock},
		{name: "enabled null", content: `{"enabled":null,"blocked_servers":["*"]}`, inviter: "@a:x.org", want: InviteBlock},
		{name: "enabled not a boolean", content: `{"enabled":"false","blocked_servers":["*"]}`, inviter: "@a:x.org", want: InviteBlock},
		// Invalid values are skipped, as in Synapse.
		{name: "field not a list", content: `{"blocked_users":"@a:x.org","blocked_servers":["x.org"]}`, inviter: "@a:x.org", want: InviteBlock},
		{name: "field not a list skipped", content: `{"blocked_users":"@a:x.org"}`, inviter: "@a:x.org", want: InviteAllow},
		{name: "non-string entries skipped", content: `{"blocked_users":[1,null,{},"@a:x.org"]}`, inviter: "@a:x.org", want: InviteBlock},
		{name: "empty glob skipped", content: `{"blocked_users":[""],"blocked_servers":[""]}`, inviter: "@a:x.org", want: InviteAllow},
		{name: "long glob skipped", content: `{"blocked_users":[` + long + `]}`, inviter: "@" + strings.Repeat("a", 260) + ":x", want: InviteAllow},
		{name: "content not an object", content: `["*"]`, inviter: "@a:x.org", want: InviteAllow},
		{name: "regex characters are literal", content: `{"blocked_users":["@a+:x.org"]}`, inviter: "@aa:x.org", want: InviteAllow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := accountDataFixture{MSC4155InvitePermissionConfigType: json.RawMessage(tc.content)}
			got, err := InviteRuleFor(context.Background(), data, "@bob:test", tc.inviter)
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestInviteRuleString(t *testing.T) {
	for rule, want := range map[InviteRule]string{InviteAllow: "allow", InviteIgnore: "ignore", InviteBlock: "block"} {
		if rule.String() != want {
			t.Fatalf("%d: got %q, want %q", rule, rule.String(), want)
		}
	}
}
