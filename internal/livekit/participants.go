// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package livekit

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// Expired participants are forgotten at most once a minute. Removing the
// participants of a member who left the room may take 30 seconds.
const (
	pruneInterval = time.Minute
	removeTimeout = 30 * time.Second
)

// Participant is a LiveKit participant on one of Zendrite's SFUs.
type Participant struct {
	Focus    *config.RTCFocus
	Room     string
	Identity string
}

// participantKey identifies a participant across SFUs.
type participantKey struct {
	sfu, room, identity string
}

func (p Participant) key() participantKey {
	return participantKey{p.Focus.URL, p.Room, p.Identity}
}

// memberKey is a member of a Matrix room.
type memberKey struct {
	roomID, userID string
}

type issued struct {
	participant Participant
	member      memberKey
	expires     time.Time
}

// participants are those Zendrite issued tokens to, until the tokens
// expire. They are kept in memory: after a restart, Zendrite cannot remove
// the holders of tokens it issued before, for at most the token lifetime.
type participants struct {
	mu        sync.Mutex
	issued    map[participantKey]*issued
	byMember  map[memberKey]map[participantKey]struct{}
	lastPrune time.Time
}

func newParticipants() participants {
	return participants{
		issued:   make(map[participantKey]*issued),
		byMember: make(map[memberKey]map[participantKey]struct{}),
	}
}

func (p *participants) add(participant Participant, roomID, userID string, expires, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Sub(p.lastPrune) >= pruneInterval {
		p.lastPrune = now
		for key, entry := range p.issued {
			if !now.Before(entry.expires) {
				p.forget(key, entry)
			}
		}
	}
	key := participant.key()
	if entry, ok := p.issued[key]; ok {
		// The identity is derived from the user, so only its token changes.
		entry.expires = expires
		return
	}
	member := memberKey{roomID, userID}
	p.issued[key] = &issued{participant: participant, member: member, expires: expires}
	if p.byMember[member] == nil {
		p.byMember[member] = make(map[participantKey]struct{})
	}
	p.byMember[member][key] = struct{}{}
}

func (p *participants) forget(key participantKey, entry *issued) {
	delete(p.issued, key)
	delete(p.byMember[entry.member], key)
	if len(p.byMember[entry.member]) == 0 {
		delete(p.byMember, entry.member)
	}
}

// of returns the participants of a member whose tokens have not expired.
func (p *participants) of(roomID, userID string, now time.Time) []Participant {
	p.mu.Lock()
	defer p.mu.Unlock()
	var found []Participant
	for key := range p.byMember[memberKey{roomID, userID}] {
		if entry := p.issued[key]; now.Before(entry.expires) {
			found = append(found, entry.participant)
		}
	}
	return found
}

// member returns the Matrix room and user of a participant whose token has
// not expired.
func (p *participants) member(sfu, room, identity string, now time.Time) (roomID, userID string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.issued[participantKey{sfu, room, identity}]
	if !ok || !now.Before(entry.expires) {
		return "", "", false
	}
	return entry.member.roomID, entry.member.userID, true
}

// Member returns the Matrix room and user of a participant on the focus's
// SFU that Zendrite issued a token to, if the token has not expired.
func (i *Issuer) Member(focus *config.RTCFocus, room, identity string) (roomID, userID string, ok bool) {
	return i.participants.member(focus.URL, room, identity, i.now())
}

// RemoveParticipant removes a participant from the SFU and revokes the
// tokens issued to them until now, which only LiveKit Cloud and LiveKit
// versions with token revocation honor. A participant who is not connected
// is not an error.
func (i *Issuer) RemoveParticipant(ctx context.Context, p Participant) error {
	err := i.call(ctx, p.Focus, "RemoveParticipant", videoGrant{Room: p.Room, RoomAdmin: true}, map[string]any{
		"room": p.Room, "identity": p.Identity, "revoke_token_ts": i.now().Unix(),
	})
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}

// Connected reports whether a participant is connected to the SFU.
func (i *Issuer) Connected(ctx context.Context, p Participant) (bool, error) {
	err := i.call(ctx, p.Focus, "GetParticipant", videoGrant{Room: p.Room, RoomAdmin: true}, map[string]string{
		"room": p.Room, "identity": p.Identity,
	})
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	return err == nil, err
}

// MembershipEnded removes a user who left or was banned from a Matrix room
// from the LiveKit rooms that Zendrite issued them tokens for, as MSC4195
// recommends, in the background. A participant may rejoin with a token that
// a self-hosted SFU cannot revoke; the SFU's webhooks let Zendrite remove
// them again.
func (i *Issuer) MembershipEnded(ctx context.Context, roomID, userID string) {
	found := i.participants.of(roomID, userID, i.now())
	if len(found) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removeTimeout)
		defer cancel()
		for _, p := range found {
			if err := i.RemoveParticipant(ctx, p); err != nil {
				logrus.WithError(err).WithField("room_id", roomID).Warn("Failed to remove a member who left the room from the LiveKit SFU")
			}
		}
	}()
}

// issuers holds the issuer of each server configuration, so that the client
// and federation APIs, which issue tokens, and the user API, which sees
// members leave, share the participants that tokens were issued to.
var issuers sync.Map

// For returns the issuer of a server configuration, the same one on every
// call with the same configuration.
func For(global *config.Global) *Issuer {
	stored, ok := issuers.Load(global)
	if !ok {
		stored, _ = issuers.LoadOrStore(global, NewIssuer(global.RTCFoci))
	}
	issuer, _ := stored.(*Issuer)
	return issuer
}
