// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package query

import (
	"context"
	"sort"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
)

// maxPartialStateHeroes is how many hero memberships a partial-state
// send_join response carries, as for the room summary of /sync.
const maxPartialStateHeroes = 5

// partialJoinState picks the state of a partial-state send_join response
// (MSC3706) out of the full state before the join, as Synapse does: every
// event that is not a membership, the joining user's own membership, and, in a
// room with neither an m.room.name nor an m.room.canonical_alias, the
// memberships of up to five heroes so that clients can name the room.
//
// Unlike Synapse, it also keeps the memberships that the join event cites as
// auth events, such as the membership of the user who authorized a restricted
// join: joining servers check the join against the returned state, and reject
// it without that membership.
//
// Heroes are joined and invited members other than the joining user, oldest
// membership first (by depth, then event ID). When there are none, they are
// members who left or were banned.
func partialJoinState(
	fullState []gomatrixserverlib.PDU, joinStateKey string, joinAuthEventIDs []string,
) []gomatrixserverlib.PDU {
	var (
		partial        []gomatrixserverlib.PDU
		present, gone  []gomatrixserverlib.PDU
		hasNameOrAlias bool
	)
	joinAuth := make(map[string]struct{}, len(joinAuthEventIDs))
	for _, id := range joinAuthEventIDs {
		joinAuth[id] = struct{}{}
	}
	for _, ev := range fullState {
		if ev.StateKeyEquals("") && (ev.Type() == spec.MRoomName || ev.Type() == spec.MRoomCanonicalAlias) {
			hasNameOrAlias = true
		}
		if ev.Type() != spec.MRoomMember {
			partial = append(partial, ev)
			continue
		}
		if _, isJoinAuth := joinAuth[ev.EventID()]; isJoinAuth || ev.StateKeyEquals(joinStateKey) {
			partial = append(partial, ev)
			continue
		}
		membership, err := ev.Membership()
		if err != nil {
			continue
		}
		switch membership {
		case spec.Join, spec.Invite:
			present = append(present, ev)
		case spec.Leave, spec.Ban:
			gone = append(gone, ev)
		}
	}
	if hasNameOrAlias {
		return partial
	}
	heroes := present
	if len(heroes) == 0 {
		heroes = gone
	}
	sort.SliceStable(heroes, func(i, j int) bool {
		if heroes[i].Depth() != heroes[j].Depth() {
			return heroes[i].Depth() < heroes[j].Depth()
		}
		return heroes[i].EventID() < heroes[j].EventID()
	})
	if len(heroes) > maxPartialStateHeroes {
		heroes = heroes[:maxPartialStateHeroes]
	}
	return append(partial, heroes...)
}

// partialJoinAuthChain returns the auth chain of the given state and of the
// join event's auth events, leaving out events that are in the state, as
// MSC3706 allows. It walks only the events in known (the full state and its
// auth chain, which contain the auth chain of every subset of the state);
// auth events that are not known are skipped, as GetAuthChain skips events it
// does not have.
func partialJoinAuthChain(
	state []gomatrixserverlib.PDU, joinAuthEventIDs []string, known []gomatrixserverlib.PDU,
) []gomatrixserverlib.PDU {
	byID := make(map[string]gomatrixserverlib.PDU, len(known))
	for _, ev := range known {
		byID[ev.EventID()] = ev
	}
	inState := make(map[string]struct{}, len(state))
	for _, ev := range state {
		inState[ev.EventID()] = struct{}{}
	}

	seen := make(map[string]struct{})
	var queue []string
	push := func(ids []string) {
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			queue = append(queue, id)
		}
	}
	push(joinAuthEventIDs)
	for _, ev := range state {
		push(ev.AuthEventIDs())
	}

	var chain []gomatrixserverlib.PDU
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		ev, ok := byID[id]
		if !ok {
			continue
		}
		push(ev.AuthEventIDs())
		if _, ok := inState[id]; !ok {
			chain = append(chain, ev)
		}
	}
	return chain
}

// serversInState returns the sorted, de-duplicated servers of the joined
// members in the given state, for servers_in_room of a partial-state
// send_join response. Members whose state key is not a user ID and does not
// resolve to one (a pseudo ID without a stored mapping) are skipped.
func (r *Queryer) serversInState(
	ctx context.Context, roomID spec.RoomID, state []gomatrixserverlib.PDU,
) []string {
	seen := make(map[spec.ServerName]struct{})
	for _, ev := range state {
		if ev.Type() != spec.MRoomMember || ev.StateKey() == nil {
			continue
		}
		membership, err := ev.Membership()
		if err != nil || membership != spec.Join {
			continue
		}
		userID, err := r.QueryUserIDForSender(ctx, roomID, spec.SenderID(*ev.StateKey()))
		if err != nil || userID == nil {
			continue
		}
		seen[userID.Domain()] = struct{}{}
	}
	servers := make([]string, 0, len(seen))
	for server := range seen {
		servers = append(servers, string(server))
	}
	sort.Strings(servers)
	return servers
}
