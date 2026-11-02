// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package input

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"

	fedapi "codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// policyServerKeyID is the key ID of every policy server signature (MSC4284).
const policyServerKeyID gomatrixserverlib.KeyID = "ed25519:policy_server"

// Policy server answers are cached per event, so that an event that comes in
// again, for example from another server or a retried transaction, does not
// cost another request. Failures to reach the policy server are not cached.
const (
	policyServerCacheTTL  = 10 * time.Minute
	policyServerCacheSize = 10000
)

// roomPolicyServer is the policy server a room's current state names.
type roomPolicyServer struct {
	via spec.ServerName
	key ed25519.PublicKey
}

// errPolicyServerUnavailable means the policy server could not be asked.
var errPolicyServerUnavailable = errors.New("the room's policy server could not be reached")

// policyServerCache remembers the policy server's signature, or its refusal,
// for an event checked against a given policy server and key.
type policyServerCache struct {
	mu      sync.Mutex
	entries map[string]policyServerCacheEntry
}

type policyServerCacheEntry struct {
	signature spec.Base64Bytes
	refusal   *fedapi.PolicyServerRefusal
	expires   time.Time
}

func (c *policyServerCache) get(key string) (policyServerCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expires) {
		return policyServerCacheEntry{}, false
	}
	return entry, true
}

func (c *policyServerCache) put(key string, entry policyServerCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.entries == nil {
		c.entries = map[string]policyServerCacheEntry{}
	}
	if len(c.entries) >= policyServerCacheSize {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
		// Still full: drop arbitrary entries, which only costs a request.
		for k := range c.entries {
			if len(c.entries) < policyServerCacheSize {
				break
			}
			delete(c.entries, k)
		}
	}
	entry.expires = now.Add(policyServerCacheTTL)
	c.entries[key] = entry
}

// isPolicyServerConfigEvent reports whether the event configures the policy
// server. Such events are never checked by the policy server, so that a room
// can always remove or replace one that stopped working (MSC4284).
func isPolicyServerConfigEvent(event gomatrixserverlib.PDU) bool {
	return (event.Type() == api.MRoomPolicy || event.Type() == api.UnstableMRoomPolicy) && event.StateKeyEquals("")
}

// isBareSelfLeave reports whether the event is a user leaving the room
// themselves (leaving, rejecting an invite or withdrawing a knock) with no
// content but the membership. A reason or any other key could carry content
// the policy server refused, so such a leave does not count.
func isBareSelfLeave(event gomatrixserverlib.PDU) bool {
	if event.Type() != spec.MRoomMember || !event.StateKeyEquals(string(event.SenderID())) {
		return false
	}
	var content map[string]json.RawMessage
	if err := json.Unmarshal(event.Content(), &content); err != nil || len(content) != 1 {
		return false
	}
	return string(content["membership"]) == `"`+spec.Leave+`"`
}

// roomPolicyServer returns the policy server that the room's current state
// names, or nil if it names none. The stable m.room.policy event takes
// precedence over the unstable one, also when it unsets the policy server. A
// policy server only counts while one of its users is joined to the room.
func (r *Inputer) roomPolicyServer(ctx context.Context, roomInfo *types.RoomInfo, roomID string) (*roomPolicyServer, error) {
	for _, eventType := range []string{api.MRoomPolicy, api.UnstableMRoomPolicy} {
		event, err := r.DB.GetStateEvent(ctx, roomID, eventType, "")
		if err != nil {
			return nil, fmt.Errorf("r.DB.GetStateEvent: %w", err)
		}
		if event == nil {
			continue
		}
		policy := parsePolicyServerContent(event.Content(), eventType == api.UnstableMRoomPolicy)
		if policy == nil {
			return nil, nil
		}
		joined, err := r.DB.GetServerInRoom(ctx, roomInfo.RoomNID, policy.via)
		if err != nil {
			return nil, fmt.Errorf("r.DB.GetServerInRoom: %w", err)
		}
		if !joined {
			return nil, nil
		}
		return policy, nil
	}
	return nil, nil
}

// parsePolicyServerContent reads the content of an m.room.policy event. It
// returns nil when the content names no valid policy server. Unstable events
// may carry the key as public_key instead of public_keys.ed25519.
func parsePolicyServerContent(content []byte, unstable bool) *roomPolicyServer {
	var policy struct {
		Via        string `json:"via"`
		PublicKeys struct {
			Ed25519 string `json:"ed25519"`
		} `json:"public_keys"`
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal(content, &policy); err != nil || policy.Via == "" {
		return nil
	}
	encodedKey := policy.PublicKeys.Ed25519
	if encodedKey == "" && unstable {
		encodedKey = policy.PublicKey
	}
	// The key is unpadded base64, but padding is tolerated like elsewhere.
	var key spec.Base64Bytes
	if err := key.Decode(strings.TrimRight(encodedKey, "=")); err != nil || len(key) != ed25519.PublicKeySize {
		return nil
	}
	if _, _, ok := spec.ParseAndValidateServerName(spec.ServerName(policy.Via)); !ok {
		return nil
	}
	return &roomPolicyServer{via: spec.ServerName(policy.Via), key: ed25519.PublicKey(key)}
}

// checkPolicyServer makes sure that the event carries a valid signature from
// the room's policy server, asking the policy server for one if it does not
// (MSC4284). It returns the event, with the new signature added if one was
// asked for. The error is a *fedapi.PolicyServerRefusal when the policy
// server refused to sign, and errPolicyServerUnavailable when it could not be
// asked or answered with an invalid signature.
func (r *Inputer) checkPolicyServer(
	ctx context.Context, roomInfo *types.RoomInfo, event gomatrixserverlib.PDU,
) (gomatrixserverlib.PDU, error) {
	if isPolicyServerConfigEvent(event) {
		return event, nil
	}
	policy, err := r.roomPolicyServer(ctx, roomInfo, event.RoomID().String())
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return event, nil
	}
	verImpl, err := gomatrixserverlib.GetRoomVersion(event.Version())
	if err != nil {
		return nil, err
	}
	if verifyPolicyServerSignature(verImpl, event.JSON(), policy) == nil {
		return event, nil
	}

	cacheKey := string(policy.via) + "|" + string(policy.key) + "|" + event.EventID()
	entry, cached := r.policyServerCache.get(cacheKey)
	if !cached {
		entry, err = r.askPolicyServer(ctx, event, policy)
		if err != nil {
			return nil, err
		}
		r.policyServerCache.put(cacheKey, entry)
	}
	if entry.refusal != nil {
		return nil, entry.refusal
	}
	return addPolicyServerSignature(verImpl, event, policy, entry.signature)
}

// askPolicyServer calls the policy server's /sign endpoint for the event.
func (r *Inputer) askPolicyServer(
	ctx context.Context, event gomatrixserverlib.PDU, policy *roomPolicyServer,
) (policyServerCacheEntry, error) {
	if r.FSAPI == nil {
		return policyServerCacheEntry{}, errPolicyServerUnavailable
	}
	signatures, err := r.FSAPI.PolicyServerSign(ctx, r.Cfg.Matrix.ServerName, policy.via, event)
	var refusal *fedapi.PolicyServerRefusal
	switch {
	case errors.As(err, &refusal):
		return policyServerCacheEntry{refusal: refusal}, nil
	case err != nil:
		return policyServerCacheEntry{}, fmt.Errorf("%w: %w", errPolicyServerUnavailable, err)
	}
	signature, ok := signatures[string(policy.via)][string(policyServerKeyID)]
	if !ok || len(signature) == 0 {
		// Unstable policy servers answer an empty object to refuse.
		return policyServerCacheEntry{refusal: &fedapi.PolicyServerRefusal{ErrCode: string(spec.ErrorForbidden)}}, nil
	}
	return policyServerCacheEntry{signature: signature}, nil
}

// addPolicyServerSignature adds the policy server's signature to the event
// and checks it. Signatures are not covered by the event ID, so the event
// keeps its ID.
func addPolicyServerSignature(
	verImpl gomatrixserverlib.IRoomVersion, event gomatrixserverlib.PDU,
	policy *roomPolicyServer, signature spec.Base64Bytes,
) (gomatrixserverlib.PDU, error) {
	var eventJSON map[string]json.RawMessage
	if err := json.Unmarshal(event.JSON(), &eventJSON); err != nil {
		return nil, err
	}
	signatures := map[string]map[string]spec.Base64Bytes{}
	if raw, ok := eventJSON["signatures"]; ok {
		if err := json.Unmarshal(raw, &signatures); err != nil {
			return nil, err
		}
	}
	if signatures[string(policy.via)] == nil {
		signatures[string(policy.via)] = map[string]spec.Base64Bytes{}
	}
	signatures[string(policy.via)][string(policyServerKeyID)] = signature
	raw, err := json.Marshal(signatures)
	if err != nil {
		return nil, err
	}
	eventJSON["signatures"] = raw
	signedJSON, err := json.Marshal(eventJSON)
	if err != nil {
		return nil, err
	}
	if signedJSON, err = gomatrixserverlib.EnforcedCanonicalJSON(signedJSON, event.Version()); err != nil {
		return nil, err
	}
	if err = verifyPolicyServerSignature(verImpl, signedJSON, policy); err != nil {
		return nil, fmt.Errorf("%w: invalid signature: %w", errPolicyServerUnavailable, err)
	}
	return verImpl.NewEventFromTrustedJSONWithEventID(event.EventID(), signedJSON, event.Redacted())
}

// verifyPolicyServerSignature checks the policy server's signature on the
// event JSON with the key from the room's current state. Like every event
// signature, it covers the redacted event.
func verifyPolicyServerSignature(verImpl gomatrixserverlib.IRoomVersion, eventJSON []byte, policy *roomPolicyServer) error {
	redacted, err := verImpl.RedactEventJSON(eventJSON)
	if err != nil {
		return err
	}
	return gomatrixserverlib.VerifyJSON(string(policy.via), policyServerKeyID, policy.key, redacted)
}
