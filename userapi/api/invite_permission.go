// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package api

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// Account data types that configure invite blocking (MSC4380, spec v1.18)
// and invite filtering (MSC4155).
const (
	InvitePermissionConfigType         = "m.invite_permission_config"
	UnstableInvitePermissionConfigType = "org.matrix.msc4380.invite_permission_config"
	// MSC4155InvitePermissionConfigType holds MSC4155's allow, ignore and
	// block lists. Like Synapse, only the unstable type is read for them:
	// the stable type is MSC4380's.
	MSC4155InvitePermissionConfigType = "org.matrix.msc4155.invite_permission_config"
)

// maxInviteGlobLength is the longest glob that is evaluated. User IDs cannot
// exceed 255 bytes, so longer globs are skipped, as Synapse does.
const maxInviteGlobLength = 255

// InviteRule is what happens to an invite under the invitee's configuration.
type InviteRule int

const (
	// InviteAllow delivers the invite.
	InviteAllow InviteRule = iota
	// InviteIgnore accepts the invite but hides it from the invitee: it is
	// not served by /sync or sliding sync and does not notify (MSC4155).
	InviteIgnore
	// InviteBlock refuses the invite with 403 M_INVITE_BLOCKED.
	InviteBlock
)

func (r InviteRule) String() string {
	switch r {
	case InviteIgnore:
		return "ignore"
	case InviteBlock:
		return "block"
	default:
		return "allow"
	}
}

// AccountDataQuerier reads a user's account data.
type AccountDataQuerier interface {
	QueryAccountData(ctx context.Context, req *QueryAccountDataRequest, res *QueryAccountDataResponse) error
}

// InviteConfig is a user's invite configuration. The nil value allows every
// invite.
type InviteConfig struct {
	// blockAll is set by MSC4380's default_action "block".
	blockAll bool
	// The MSC4155 lists, in evaluation order.
	allowedUsers, ignoredUsers, blockedUsers       []*regexp.Regexp
	allowedServers, ignoredServers, blockedServers []*regexp.Regexp
}

// BlocksAll reports whether every invite is blocked (MSC4380).
func (c *InviteConfig) BlocksAll() bool {
	return c != nil && c.blockAll
}

// Rule returns what happens to an invite sent by inviter. User lists are
// checked before server lists, each in the order allow, ignore, block; the
// first match decides. Without a match the invite is allowed.
func (c *InviteConfig) Rule(inviter string) InviteRule {
	if c == nil {
		return InviteAllow
	}
	if c.blockAll {
		return InviteBlock
	}
	for _, list := range []struct {
		globs []*regexp.Regexp
		rule  InviteRule
	}{
		{c.allowedUsers, InviteAllow},
		{c.ignoredUsers, InviteIgnore},
		{c.blockedUsers, InviteBlock},
	} {
		if matchAny(list.globs, inviter) {
			return list.rule
		}
	}
	_, domain, ok := strings.Cut(inviter, ":")
	if !ok || domain == "" {
		return InviteAllow
	}
	host := stripPort(domain)
	for _, list := range []struct {
		globs []*regexp.Regexp
		rule  InviteRule
	}{
		{c.allowedServers, InviteAllow},
		{c.ignoredServers, InviteIgnore},
		{c.blockedServers, InviteBlock},
	} {
		// The MSC matches server globs against the server name without its
		// port; Synapse matches the whole server name. Either matches here.
		if matchAny(list.globs, host) || (host != domain && matchAny(list.globs, domain)) {
			return list.rule
		}
	}
	return InviteAllow
}

func matchAny(globs []*regexp.Regexp, s string) bool {
	for _, glob := range globs {
		if glob.MatchString(s) {
			return true
		}
	}
	return false
}

// stripPort removes a port suffix from a server name, minding IPv6 literals.
func stripPort(serverName string) string {
	i := strings.LastIndexByte(serverName, ':')
	if i < 0 || i < strings.LastIndexByte(serverName, ']') {
		return serverName
	}
	return serverName[:i]
}

// compileInviteGlob turns a Matrix glob into a case-insensitive regular
// expression matching the whole string: "*" matches any number of characters
// and "?" exactly one.
func compileInviteGlob(glob string) (*regexp.Regexp, error) {
	escaped := regexp.QuoteMeta(glob)
	escaped = strings.ReplaceAll(escaped, `\*`, ".*")
	escaped = strings.ReplaceAll(escaped, `\?`, ".")
	return regexp.Compile(`(?is)\A` + escaped + `\z`)
}

// parseMSC4155 reads MSC4155 content. Like Synapse, fields that are not
// arrays and entries that are not non-empty strings are skipped instead of
// invalidating the whole configuration.
func parseMSC4155(data json.RawMessage) *InviteConfig {
	var content map[string]json.RawMessage
	if err := json.Unmarshal(data, &content); err != nil || content == nil {
		return nil
	}
	// "enabled": false switches the configuration off (MSC4155).
	var enabled *bool
	if json.Unmarshal(content["enabled"], &enabled) == nil && enabled != nil && !*enabled {
		return nil
	}
	globs := func(field string) []*regexp.Regexp {
		var values []any
		if err := json.Unmarshal(content[field], &values); err != nil {
			return nil
		}
		var out []*regexp.Regexp
		for _, value := range values {
			s, ok := value.(string)
			if !ok || s == "" || len(s) > maxInviteGlobLength {
				continue
			}
			if re, err := compileInviteGlob(s); err == nil {
				out = append(out, re)
			}
		}
		return out
	}
	return &InviteConfig{
		allowedUsers:   globs("allowed_users"),
		ignoredUsers:   globs("ignored_users"),
		blockedUsers:   globs("blocked_users"),
		allowedServers: globs("allowed_servers"),
		ignoredServers: globs("ignored_servers"),
		blockedServers: globs("blocked_servers"),
	}
}

// nonEmptyObject reports whether data is a JSON object with at least one key.
func nonEmptyObject(data json.RawMessage) (map[string]json.RawMessage, bool) {
	var content map[string]json.RawMessage
	if err := json.Unmarshal(data, &content); err != nil || len(content) == 0 {
		return nil, false
	}
	return content, true
}

func queryGlobalAccountData(ctx context.Context, q AccountDataQuerier, userID, dataType string) (json.RawMessage, error) {
	var res QueryAccountDataResponse
	if err := q.QueryAccountData(ctx, &QueryAccountDataRequest{UserID: userID, DataType: dataType}, &res); err != nil {
		return nil, err
	}
	data, ok := res.GlobalAccountData[dataType]
	if !ok || len(data) == 0 {
		return nil, nil
	}
	return data, nil
}

// LoadInviteConfig reads the local user's invite configuration, in Synapse's
// order:
//
//  1. m.invite_permission_config (MSC4380) with any content: default_action
//     "block" blocks every invite, anything else allows every invite. The
//     unstable MSC4380 type is read when the stable one is absent.
//  2. Otherwise org.matrix.msc4155.invite_permission_config (MSC4155).
//  3. Otherwise every invite is allowed.
//
// Empty or non-object MSC4380 content states no preference. A present stable
// MSC4380 type hides the unstable one even when empty.
func LoadInviteConfig(ctx context.Context, q AccountDataQuerier, userID string) (*InviteConfig, error) {
	if q == nil {
		return nil, nil
	}
	for _, dataType := range []string{InvitePermissionConfigType, UnstableInvitePermissionConfigType} {
		data, err := queryGlobalAccountData(ctx, q, userID, dataType)
		if err != nil {
			return nil, err
		}
		if data == nil {
			continue
		}
		if content, ok := nonEmptyObject(data); ok {
			var action any
			_ = json.Unmarshal(content["default_action"], &action)
			if action == "block" {
				return &InviteConfig{blockAll: true}, nil
			}
			return nil, nil
		}
		break
	}
	data, err := queryGlobalAccountData(ctx, q, userID, MSC4155InvitePermissionConfigType)
	if err != nil || data == nil {
		return nil, err
	}
	return parseMSC4155(data), nil
}

// InviteRuleFor returns what happens to an invite from inviter to the local
// user invitee.
func InviteRuleFor(ctx context.Context, q AccountDataQuerier, invitee, inviter string) (InviteRule, error) {
	config, err := LoadInviteConfig(ctx, q, invitee)
	if err != nil {
		return InviteAllow, err
	}
	return config.Rule(inviter), nil
}
