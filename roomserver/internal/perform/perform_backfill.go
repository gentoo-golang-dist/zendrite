// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package perform

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/sirupsen/logrus"

	federationAPI "codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/auth"
	"codefloe.com/pat-s/zendrite/roomserver/internal/helpers"
	"codefloe.com/pat-s/zendrite/roomserver/state"
	"codefloe.com/pat-s/zendrite/roomserver/storage"
	"codefloe.com/pat-s/zendrite/roomserver/types"
)

// the max number of servers to backfill from per request. If this is too low we may fail to backfill when
// we could've from another server. If this is too high we may take far too long to successfully backfill
// as we try dead servers.
const maxBackfillServers = 5

type Backfiller struct {
	IsLocalServerName func(spec.ServerName) bool
	DB                storage.Database
	FSAPI             federationAPI.RoomserverFederationAPI
	KeyRing           gomatrixserverlib.JSONVerifier
	Querier           api.QuerySenderIDAPI

	// The servers which should be preferred above other servers when backfilling
	PreferServers []spec.ServerName
}

// PerformBackfill implements api.RoomServerQueryAPI.
func (r *Backfiller) PerformBackfill(
	ctx context.Context,
	request *api.PerformBackfillRequest,
	response *api.PerformBackfillResponse,
) error {
	// if we are requesting the backfill then we need to do a federation hit
	// TODO: we could be more sensible and fetch as many events we already have then request the rest
	//       which is what the syncapi does already.
	if r.IsLocalServerName(request.ServerName) {
		return r.backfillViaFederation(ctx, request, response)
	}
	// someone else is requesting the backfill, try to service their request.
	var err error
	var front []string

	// The limit defines the maximum number of events to retrieve, so it also
	// defines the highest number of elements in the map below.
	visited := make(map[string]bool, request.Limit)

	// this will include these events which is what we want
	front = request.PrevEventIDs()

	info, err := r.DB.RoomInfo(ctx, request.RoomID)
	if err != nil {
		return err
	}
	if info == nil || info.IsStub() {
		return fmt.Errorf("PerformBackfill: missing room info for room %s", request.RoomID)
	}

	// Scan the event tree for events to send back.
	resultNIDs, redactEventIDs, err := helpers.ScanEventTree(ctx, r.DB, info, front, visited, request.Limit, request.ServerName, r.Querier)
	if err != nil {
		return err
	}

	// Retrieve events from the list that was filled previously. If we fail to get
	// events from the database then attempt once to get them from federation instead.
	var loadedEvents []gomatrixserverlib.PDU
	loadedEvents, err = helpers.LoadEvents(ctx, r.DB, info, resultNIDs)
	if err != nil {
		var missingEventErr types.MissingEventError
		if errors.As(err, &missingEventErr) {
			return r.backfillViaFederation(ctx, request, response)
		}
		return err
	}

	for _, event := range loadedEvents {
		if _, ok := redactEventIDs[event.EventID()]; ok {
			// Loaded events can be shared through the event cache, so
			// redact a copy. A copy that cannot be made is left out.
			redacted, redactErr := types.RedactedCopy(event)
			if redactErr != nil {
				continue
			}
			event = redacted
		}
		response.Events = append(response.Events, &types.HeaderedEvent{PDU: event})
	}

	return err
}

func (r *Backfiller) backfillViaFederation(ctx context.Context, req *api.PerformBackfillRequest, res *api.PerformBackfillResponse) error {
	info, err := r.DB.RoomInfo(ctx, req.RoomID)
	if err != nil {
		return err
	}
	if info == nil || info.IsStub() {
		return fmt.Errorf("backfillViaFederation: missing room info for room %s", req.RoomID)
	}
	requester := newBackfillRequester(r.DB, r.FSAPI, r.Querier, r.KeyRing, req.RoomID, req.VirtualHost, r.IsLocalServerName, req.BackwardsExtremities, r.PreferServers, info.RoomVersion)
	// Request 100 items regardless of what the query asks for.
	// We don't want to go much higher than this.
	// We can't honor exactly the limit as some sytests rely on requesting more for tests to pass
	// (so we don't need to hit /state_ids which the test has no listener for)
	// Specifically the test "Outbound federation can backfill events"
	events, err := gomatrixserverlib.RequestBackfill(
		ctx, req.VirtualHost, requester,
		r.KeyRing, req.RoomID, info.RoomVersion, req.PrevEventIDs(), 100, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) { //nolint:mnd
			return r.Querier.QueryUserIDForSender(ctx, roomID, senderID)
		},
	)
	// Only return an error if we really couldn't get any events.
	if err != nil && len(events) == 0 {
		logrus.WithError(err).Errorf("gomatrixserverlib.RequestBackfill failed")
		return err
	}
	// If we got an error but still got events, that's fine, because a server might have returned a 404 (or something)
	// but other servers could provide the missing event.
	logrus.WithError(err).WithField("room_id", req.RoomID).WithField("received", requester.received).Infof("backfilled %d events", len(events))

	// Requesting events from federation can take a while, during which the room
	// may have been purged. Re-check before persisting so we neither resurrect
	// the room locally nor hand the events back to the caller (e.g. the sync API)
	// to store into the purged room (#224).
	if info, err = r.DB.RoomInfo(ctx, req.RoomID); err != nil {
		return err
	}
	if info == nil || info.IsStub() {
		logrus.WithField("room_id", req.RoomID).Warn("Discarding backfilled events: room no longer exists, it was likely purged")
		return nil
	}

	// The auth events go first, so that the events can refer to them.
	persistEvents(ctx, r.DB, requester.fetchedAuthEventsOf(events))

	// persist these new events - auth checks have already been done
	roomNID, storedEvents := persistEvents(ctx, r.DB, events)

	for _, ev := range storedEvents {
		// now add state for these events
		stateIDs, ok := requester.eventIDToBeforeStateIDs[ev.EventID()]
		if !ok {
			// this should be impossible as all events returned must have pass Step 5 of the PDU checks
			// which requires a list of state IDs.
			logrus.WithError(err).WithField("event_id", ev.EventID()).Error("backfillViaFederation: failed to find state IDs for event which passed auth checks")
			continue
		}
		var entries []types.StateEntry
		if entries, err = r.DB.StateEntriesForEventIDs(ctx, stateIDs, true); err != nil {
			// attempt to fetch the missing events
			r.fetchAndStoreMissingEvents(ctx, info, requester, stateIDs, req.VirtualHost)
			// try again
			entries, err = r.DB.StateEntriesForEventIDs(ctx, stateIDs, true)
			if err != nil {
				logrus.WithError(err).WithField("event_id", ev.EventID()).Error("backfillViaFederation: failed to get state entries for event")
				return err
			}
		}

		var beforeStateSnapshotNID types.StateSnapshotNID
		if beforeStateSnapshotNID, err = r.DB.AddState(ctx, roomNID, nil, entries); err != nil {
			logrus.WithError(err).WithField("event_id", ev.EventID()).Error("backfillViaFederation: failed to persist state entries to get snapshot nid")
			return err
		}
		if err = r.DB.SetState(ctx, ev.EventNID, beforeStateSnapshotNID); err != nil {
			logrus.WithError(err).WithField("event_id", ev.EventID()).Error("backfillViaFederation: failed to persist snapshot nid")
		}
	}

	res.Redactions = applyBackfilledRedactions(ctx, r.DB, r.Querier, info, storedEvents, events)

	// TODO: update backwards extremities, as that should be moved from syncapi to roomserver at some point.

	res.Events = make([]*types.HeaderedEvent, len(events))
	for i := range events {
		res.Events[i] = &types.HeaderedEvent{PDU: events[i]}
	}
	res.HistoryVisibility = requester.historyVisiblity
	return nil
}

// fetchAndStoreMissingEvents does a best-effort fetch and store of missing events specified in stateIDs. Returns no error as it is just
// best effort.
func (r *Backfiller) fetchAndStoreMissingEvents(ctx context.Context, info *types.RoomInfo,
	backfillRequester *backfillRequester, stateIDs []string, virtualHost spec.ServerName,
) {
	servers := backfillRequester.servers

	// work out which are missing
	nidMap, err := r.DB.EventNIDs(ctx, stateIDs)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Warn("cannot query missing events")
		return
	}
	missingMap := make(map[string]*types.HeaderedEvent) // id -> event
	for _, id := range stateIDs {
		if _, ok := nidMap[id]; !ok {
			missingMap[id] = nil
		}
	}
	util.GetLogger(ctx).Infof("Fetching %d missing state events (from %d possible servers)", len(missingMap), len(servers))

	// fetch the events from federation. Loop the servers first so if we find one that works we stick with them
	for _, srv := range servers {
		for id, ev := range missingMap {
			if ev != nil {
				continue // already found
			}
			logger := util.GetLogger(ctx).WithField("server", srv).WithField("event_id", id)
			res, err := r.FSAPI.GetEvent(ctx, virtualHost, srv, id)
			if err != nil {
				logger.WithError(err).Warn("failed to get event from server")
				continue
			}
			loader := gomatrixserverlib.NewEventsLoader(info.RoomVersion, r.KeyRing, backfillRequester, backfillRequester.ProvideEvents, false)
			result, err := loader.LoadAndVerify(ctx, res.PDUs, gomatrixserverlib.TopologicalOrderByPrevEvents, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
				return r.Querier.QueryUserIDForSender(ctx, roomID, senderID)
			})
			if err != nil {
				logger.WithError(err).Warn("failed to load and verify event")
				continue
			}
			logger.Infof("returned %d PDUs which made events %+v", len(res.PDUs), result)
			for _, res := range result {
				var sigErr gomatrixserverlib.SignatureErr
				var authChainErr gomatrixserverlib.AuthChainErr
				var authRulesErr gomatrixserverlib.AuthRulesErr
				switch {
				case res.Error == nil:
				case errors.As(res.Error, &sigErr):
					// The signature of the event might not be valid anymore, for example if
					// the key ID was reused with a different signature.
					logger.WithError(res.Error).Errorf("event failed PDU checks, storing anyway")
				case errors.As(res.Error, &authChainErr) || errors.As(res.Error, &authRulesErr):
					logger.WithError(res.Error).Warn("event failed PDU checks")
					continue
				default:
					logger.WithError(err).Warn("event failed PDU checks")
					continue
				}
				missingMap[id] = &types.HeaderedEvent{PDU: res.Event}
			}
		}
	}

	var newEvents []gomatrixserverlib.PDU
	for _, ev := range missingMap {
		if ev != nil {
			newEvents = append(newEvents, ev.PDU)
		}
	}
	util.GetLogger(ctx).Infof("Persisting %d new events", len(newEvents))
	_, stored := persistEvents(ctx, r.DB, newEvents)
	applyBackfilledRedactions(ctx, r.DB, r.Querier, info, stored, newEvents)
}

// backfillRequester implements gomatrixserverlib.BackfillRequester.
type backfillRequester struct {
	db                storage.Database
	fsAPI             federationAPI.RoomserverFederationAPI
	querier           api.QuerySenderIDAPI
	keyRing           gomatrixserverlib.JSONVerifier
	roomID            string
	virtualHost       spec.ServerName
	isLocalServerName func(spec.ServerName) bool
	preferServer      map[spec.ServerName]bool
	bwExtrems         map[string][]string

	// per-request state
	servers                 []spec.ServerName
	eventIDToBeforeStateIDs map[string][]string
	eventIDMap              map[string]gomatrixserverlib.PDU
	historyVisiblity        gomatrixserverlib.HistoryVisibility
	roomVersion             gomatrixserverlib.RoomVersion
	// received counts the events other servers sent, before any checks.
	received int
	// fetchedAuthEvents holds the auth events fetched from other servers, with
	// a nil entry for those no server could provide.
	fetchedAuthEvents map[string]gomatrixserverlib.PDU
}

func newBackfillRequester(
	db storage.Database, fsAPI federationAPI.RoomserverFederationAPI,
	querier api.QuerySenderIDAPI,
	keyRing gomatrixserverlib.JSONVerifier,
	roomID string,
	virtualHost spec.ServerName,
	isLocalServerName func(spec.ServerName) bool,
	bwExtrems map[string][]string, preferServers []spec.ServerName,
	roomVersion gomatrixserverlib.RoomVersion,
) *backfillRequester {
	preferServer := make(map[spec.ServerName]bool)
	for _, p := range preferServers {
		preferServer[p] = true
	}
	return &backfillRequester{
		db:                      db,
		fsAPI:                   fsAPI,
		querier:                 querier,
		keyRing:                 keyRing,
		roomID:                  roomID,
		virtualHost:             virtualHost,
		isLocalServerName:       isLocalServerName,
		eventIDToBeforeStateIDs: make(map[string][]string),
		eventIDMap:              make(map[string]gomatrixserverlib.PDU),
		bwExtrems:               bwExtrems,
		preferServer:            preferServer,
		historyVisiblity:        gomatrixserverlib.HistoryVisibilityShared,
		roomVersion:             roomVersion,
		fetchedAuthEvents:       make(map[string]gomatrixserverlib.PDU),
	}
}

func (b *backfillRequester) StateIDsBeforeEvent(ctx context.Context, targetEvent gomatrixserverlib.PDU) ([]string, error) {
	b.eventIDMap[targetEvent.EventID()] = targetEvent
	if ids, ok := b.eventIDToBeforeStateIDs[targetEvent.EventID()]; ok {
		return ids, nil
	}
	if len(targetEvent.PrevEventIDs()) == 0 && targetEvent.Type() == "m.room.create" && targetEvent.StateKeyEquals("") {
		util.GetLogger(ctx).WithField("room_id", targetEvent.RoomID().String()).Info("Backfilled to the beginning of the room")
		b.eventIDToBeforeStateIDs[targetEvent.EventID()] = []string{}
		return nil, nil
	}
	// if we have exactly 1 prev event and we know the state of the room at that prev event, then just roll forward the prev event.
	// Else, we have to hit /state_ids because either we don't know the state at all at this event (new backwards extremity) or
	// we don't know the result of state res to merge forks (2 or more prev_events)
	if len(targetEvent.PrevEventIDs()) == 1 {
		prevEventID := targetEvent.PrevEventIDs()[0]
		prevEvent, ok := b.eventIDMap[prevEventID]
		if !ok {
			goto FederationHit
		}
		prevEventStateIDs, ok := b.eventIDToBeforeStateIDs[prevEventID]
		if !ok {
			goto FederationHit
		}
		newStateIDs := b.calculateNewStateIDs(targetEvent, prevEvent, prevEventStateIDs)
		if newStateIDs != nil {
			b.eventIDToBeforeStateIDs[targetEvent.EventID()] = newStateIDs
			return newStateIDs, nil
		}
		// else we failed to calculate the new state, so fallthrough
	}

FederationHit:
	var lastErr error
	logrus.WithField("event_id", targetEvent.EventID()).Info("Requesting /state_ids at event")
	for _, srv := range b.servers { // hit any valid server
		c := gomatrixserverlib.FederatedStateProvider{
			FedClient:          b.fsAPI,
			RememberAuthEvents: false,
			Server:             srv,
			Origin:             b.virtualHost,
		}
		res, err := c.StateIDsBeforeEvent(ctx, targetEvent)
		if err != nil {
			lastErr = err
			continue
		}
		b.eventIDToBeforeStateIDs[targetEvent.EventID()] = res
		return res, nil
	}
	return nil, lastErr
}

func (b *backfillRequester) calculateNewStateIDs(targetEvent, prevEvent gomatrixserverlib.PDU, prevEventStateIDs []string) []string {
	// A copy, since the state before the prev event must stay as it is.
	newStateIDs := slices.Clone(prevEventStateIDs)
	if prevEvent.StateKey() == nil {
		// state is the same as the previous event
		b.eventIDToBeforeStateIDs[targetEvent.EventID()] = newStateIDs
		return newStateIDs
	}

	missingState := false // true if we are missing the info for a state event ID
	foundEvent := false   // true if we found a (type, state_key) match
	// find which state ID to replace, if any
	for i, id := range newStateIDs {
		ev, ok := b.eventIDMap[id]
		if !ok {
			missingState = true
			continue
		}
		// The state IDs BEFORE the target event are the state IDs BEFORE the prev_event PLUS the prev_event itself
		if ev.Type() == prevEvent.Type() && ev.StateKeyEquals(*prevEvent.StateKey()) {
			newStateIDs[i] = prevEvent.EventID()
			foundEvent = true
			break
		}
	}
	if !foundEvent && !missingState {
		// we can be certain that this is new state
		newStateIDs = append(newStateIDs, prevEvent.EventID())
		foundEvent = true
	}

	if foundEvent {
		b.eventIDToBeforeStateIDs[targetEvent.EventID()] = newStateIDs
		return newStateIDs
	}
	return nil
}

func (b *backfillRequester) StateBeforeEvent(ctx context.Context, roomVer gomatrixserverlib.RoomVersion,
	event gomatrixserverlib.PDU, eventIDs []string,
) (map[string]gomatrixserverlib.PDU, error) {
	// Events an earlier call fetched are not stored, and the state at the next
	// event usually holds the same ones. Without them the whole state of the
	// room would be requested again for every event.
	known := make(map[string]gomatrixserverlib.PDU, len(eventIDs))
	var unknownIDs []string
	for _, id := range eventIDs {
		if ev, ok := b.eventIDMap[id]; ok {
			known[id] = ev
		} else {
			unknownIDs = append(unknownIDs, id)
		}
	}
	if len(unknownIDs) == 0 {
		return known, nil
	}

	// try to fetch the events from the database first
	events, err := b.storedEvents(unknownIDs) //nolint:contextcheck
	if err != nil {
		// non-fatal, fallthrough
		logrus.WithError(err).Info("Failed to fetch events")
	} else {
		logrus.Infof("Fetched %d/%d events from the database", len(events), len(unknownIDs))
		for i := range events {
			known[events[i].EventID()] = events[i]
			b.eventIDMap[events[i].EventID()] = events[i]
		}
		if len(known) == len(eventIDs) {
			return known, nil
		}
	}

	var lastErr error
	for _, srv := range b.servers {
		c := gomatrixserverlib.FederatedStateProvider{
			FedClient:          b.fsAPI,
			RememberAuthEvents: false,
			Server:             srv,
			Origin:             b.virtualHost,
		}
		result, err := c.StateBeforeEvent(ctx, roomVer, event, eventIDs)
		if err != nil {
			lastErr = err
			continue
		}
		for eventID, ev := range result {
			b.eventIDMap[eventID] = ev
		}
		for eventID, ev := range known {
			result[eventID] = ev
		}
		return result, nil
	}
	return nil, lastErr
}

// ServersAtEvent is called when trying to determine which server to request from.
// It returns a list of servers which can be queried for backfill requests. These servers
// will be servers that are in the room already. The entries at the beginning are preferred servers
// and will be tried first. An empty list will fail the request.
func (b *backfillRequester) ServersAtEvent(ctx context.Context, roomID, eventID string) []spec.ServerName {
	// eventID will be a prev_event ID of a backwards extremity, meaning we will not have a database entry for it. Instead, use
	// its successor, so look it up.
	successor := ""
FindSuccessor:
	for sucID, prevEventIDs := range b.bwExtrems {
		for _, pe := range prevEventIDs {
			if pe == eventID {
				successor = sucID
				break FindSuccessor
			}
		}
	}
	if successor == "" {
		logrus.WithField("event_id", eventID).Error("ServersAtEvent: failed to find successor of this event to determine room state")
		return nil
	}
	eventID = successor

	// getMembershipsBeforeEventNID requires a NID, so retrieving the NID for
	// the event is necessary.
	NIDs, err := b.db.EventNIDs(ctx, []string{eventID})
	if err != nil {
		logrus.WithField("event_id", eventID).WithError(err).Error("ServersAtEvent: failed to get event NID for event")
		return nil
	}

	info, err := b.db.RoomInfo(ctx, roomID)
	if err != nil {
		logrus.WithError(err).WithField("room_id", roomID).Error("ServersAtEvent: failed to get RoomInfo for room")
		return nil
	}
	if info == nil || info.IsStub() {
		logrus.WithField("room_id", roomID).Error("ServersAtEvent: failed to get RoomInfo for room, room is missing")
		return nil
	}

	stateEntries, err := helpers.StateBeforeEvent(ctx, b.db, info, NIDs[eventID].EventNID, b.querier)
	if err != nil {
		logrus.WithField("event_id", eventID).WithError(err).Error("ServersAtEvent: failed to load state before event")
		return nil
	}

	// possibly return all joined servers depending on history visiblity
	memberEventsFromVis, visibility, err := joinEventsFromHistoryVisibility(ctx, b.db, b.querier, info, stateEntries, b.virtualHost)
	b.historyVisiblity = visibility
	if err != nil {
		logrus.WithError(err).Error("ServersAtEvent: failed calculate servers from history visibility rules")
		return nil
	}
	logrus.Infof("ServersAtEvent including %d current events from history visibility", len(memberEventsFromVis))

	// Retrieve all "m.room.member" state events of "join" membership, which
	// contains the list of users in the room before the event, therefore all
	// the servers in it at that moment.
	memberEvents, err := helpers.GetMembershipsAtState(ctx, b.db, info, stateEntries, true)
	if err != nil {
		logrus.WithField("event_id", eventID).WithError(err).Error("ServersAtEvent: failed to get memberships before event")
		return nil
	}
	memberEvents = append(memberEvents, memberEventsFromVis...)

	// Store the server names in a temporary map to avoid duplicates.
	serverSet := make(map[spec.ServerName]bool)
	for _, event := range memberEvents {
		if sender, err := b.querier.QueryUserIDForSender(ctx, event.RoomID(), event.SenderID()); err == nil {
			serverSet[sender.Domain()] = true
		}
	}
	var servers []spec.ServerName
	for server := range serverSet {
		if b.isLocalServerName(server) {
			continue
		}
		if b.preferServer[server] { // insert at the front
			servers = append([]spec.ServerName{server}, servers...)
		} else { // insert at the back
			servers = append(servers, server)
		}
	}
	if len(servers) > maxBackfillServers {
		servers = servers[:maxBackfillServers]
	}

	b.servers = servers
	return servers
}

// Backfill performs a backfill request to the given server.
// https://matrix.org/docs/spec/server_server/latest#get-matrix-federation-v1-backfill-roomid
func (b *backfillRequester) Backfill(ctx context.Context, origin, server spec.ServerName, roomID string,
	limit int, fromEventIDs []string,
) (gomatrixserverlib.Transaction, error) {
	tx, err := b.fsAPI.Backfill(ctx, origin, server, roomID, limit, fromEventIDs)
	b.received += len(tx.PDUs)
	return tx, err
}

// ProvideEvents returns the given auth events of an event being verified. A
// joining server only gets the auth chain of the current state, which lacks
// for example the memberships of those who have left, so the events that are
// not stored are fetched. Their signatures are verified here, and their own
// auth events are verified by the caller.
func (b *backfillRequester) ProvideEvents(_ gomatrixserverlib.RoomVersion, eventIDs []string) ([]gomatrixserverlib.PDU, error) {
	events, err := b.storedEvents(eventIDs)
	if err != nil {
		return nil, err
	}
	stored := make(map[string]bool, len(events))
	for _, ev := range events {
		stored[ev.EventID()] = true
	}
	for _, id := range eventIDs {
		if stored[id] {
			continue
		}
		if ev := b.fetchAuthEvent(id); ev != nil {
			events = append(events, ev)
		}
	}
	return events, nil
}

// fetchAuthEvent fetches an event from the servers of this backfill, once per
// backfill. It returns nil if no server can provide it.
func (b *backfillRequester) fetchAuthEvent(eventID string) gomatrixserverlib.PDU {
	if ev, ok := b.fetchedAuthEvents[eventID]; ok {
		return ev
	}
	ctx := context.Background()
	logger := logrus.WithField("room_id", b.roomID).WithField("event_id", eventID)
	var event gomatrixserverlib.PDU
	for _, srv := range b.servers {
		res, err := b.fsAPI.GetEvent(ctx, b.virtualHost, srv, eventID)
		if err != nil {
			logger.WithError(err).WithField("server", srv).Debug("Failed to fetch auth event of backfilled event")
			continue
		}
		if event = b.verifiedAuthEvent(ctx, res, eventID); event != nil {
			break
		}
		logger.WithField("server", srv).Warn("Server sent an invalid auth event for a backfilled event")
	}
	b.fetchedAuthEvents[eventID] = event
	return event
}

// verifiedAuthEvent returns the wanted event from a response, if it is in this
// room and its signatures are valid.
func (b *backfillRequester) verifiedAuthEvent(ctx context.Context, res gomatrixserverlib.Transaction, eventID string) gomatrixserverlib.PDU {
	if len(res.PDUs) != 1 {
		return nil
	}
	verImpl, err := gomatrixserverlib.GetRoomVersion(b.roomVersion)
	if err != nil {
		return nil
	}
	ev, err := verImpl.NewEventFromUntrustedJSON(res.PDUs[0])
	if err != nil || ev.EventID() != eventID || ev.RoomID().String() != b.roomID {
		return nil
	}
	if err = gomatrixserverlib.VerifyEventSignatures(ctx, ev, b.keyRing, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
		return b.querier.QueryUserIDForSender(ctx, roomID, senderID)
	}); err != nil {
		return nil
	}
	return ev
}

// fetchedAuthEventsOf returns the fetched events in the auth chains of the
// given events, each before the events that need it. Being in the auth chain
// of an event that passed its checks, they passed those checks as well.
func (b *backfillRequester) fetchedAuthEventsOf(events []gomatrixserverlib.PDU) []gomatrixserverlib.PDU {
	var result []gomatrixserverlib.PDU
	seen := make(map[string]bool)
	var visit func(ev gomatrixserverlib.PDU)
	visit = func(ev gomatrixserverlib.PDU) {
		for _, id := range ev.AuthEventIDs() {
			if authEvent := b.fetchedAuthEvents[id]; authEvent != nil && !seen[id] {
				seen[id] = true
				visit(authEvent)
				result = append(result, authEvent)
			}
		}
	}
	for _, ev := range events {
		visit(ev)
	}
	return result
}

// storedEvents returns those of the given events this server has stored.
func (b *backfillRequester) storedEvents(eventIDs []string) ([]gomatrixserverlib.PDU, error) {
	ctx := context.Background()
	nidMap, err := b.db.EventNIDs(ctx, eventIDs)
	if err != nil {
		logrus.WithError(err).WithField("event_ids", eventIDs).Error("Failed to find events")
		return nil, err
	}
	eventNIDs := make([]types.EventNID, len(nidMap))
	i := 0
	for _, nid := range nidMap {
		eventNIDs[i] = nid.EventNID
		i++
	}
	eventsWithNids, err := b.db.Events(ctx, b.roomVersion, eventNIDs)
	if err != nil {
		logrus.WithError(err).WithField("event_nids", eventNIDs).Error("Failed to load events")
		return nil, err
	}
	events := make([]gomatrixserverlib.PDU, len(eventsWithNids))
	for i := range eventsWithNids {
		events[i] = eventsWithNids[i].PDU
	}
	return events, nil
}

// joinEventsFromHistoryVisibility returns all CURRENTLY joined members if our server can read the room history
//
// TODO: Long term we probably want a history_visibility table which stores eventNID | visibility_enum so we can just
// pull all events and then filter by that table.
func joinEventsFromHistoryVisibility(
	ctx context.Context, db storage.RoomDatabase, querier api.QuerySenderIDAPI, roomInfo *types.RoomInfo, stateEntries []types.StateEntry,
	thisServer spec.ServerName,
) ([]types.Event, gomatrixserverlib.HistoryVisibility, error) {
	var eventNIDs []types.EventNID
	for _, entry := range stateEntries {
		// Filter the events to retrieve to only keep the membership events
		if entry.EventTypeNID == types.MRoomHistoryVisibilityNID && entry.EventStateKeyNID == types.EmptyStateKeyNID {
			eventNIDs = append(eventNIDs, entry.EventNID)
			break
		}
	}

	// Get all of the events in this state
	if roomInfo == nil {
		return nil, gomatrixserverlib.HistoryVisibilityJoined, types.ErrorInvalidRoomInfo
	}
	stateEvents, err := db.Events(ctx, roomInfo.RoomVersion, eventNIDs)
	if err != nil {
		// even though the default should be shared, restricting the visibility to joined
		// feels more secure here.
		return nil, gomatrixserverlib.HistoryVisibilityJoined, err
	}
	events := make([]gomatrixserverlib.PDU, len(stateEvents))
	for i := range stateEvents {
		events[i] = stateEvents[i].PDU
	}

	// Can we see events in the room?
	canSeeEvents := auth.IsServerAllowed(ctx, querier, thisServer, true, events)
	visibility := auth.HistoryVisibilityForRoom(events)
	if !canSeeEvents {
		logrus.Infof("ServersAtEvent history not visible to us: %s", visibility)
		return nil, visibility, nil
	}
	// get joined members
	joinEventNIDs, err := db.GetMembershipEventNIDsForRoom(ctx, roomInfo.RoomNID, true, false)
	if err != nil {
		return nil, visibility, err
	}
	evs, err := db.Events(ctx, roomInfo.RoomVersion, joinEventNIDs)
	return evs, visibility, err
}

func persistEvents(ctx context.Context, db storage.Database, events []gomatrixserverlib.PDU) (types.RoomNID, []types.Event) {
	var roomNID types.RoomNID
	var eventNID types.EventNID
	storedEvents := make([]types.Event, 0, len(events))
	for _, ev := range events {
		nidMap, err := db.EventNIDs(ctx, ev.AuthEventIDs())
		if err != nil { // this shouldn't happen as RequestBackfill already found them
			logrus.WithError(err).WithField("auth_events", ev.AuthEventIDs()).Error("Failed to find one or more auth events")
			continue
		}
		authNids := make([]types.EventNID, len(nidMap))
		i := 0
		for _, nid := range nidMap {
			authNids[i] = nid.EventNID
			i++
		}

		// Look the room up rather than creating it: backfill only ever adds
		// historical events to a room we already have. If the room is missing
		// (e.g. it was purged while this backfill was in flight) we must not
		// re-create it, otherwise the purged room is silently resurrected (#224).
		roomInfo, err := db.RoomInfo(ctx, ev.RoomID().String())
		if err != nil {
			logrus.WithError(err).Error("failed to look up roomNID")
			continue
		}
		if roomInfo == nil || roomInfo.IsStub() {
			logrus.WithField("room_id", ev.RoomID().String()).WithField("event_id", ev.EventID()).
				Warn("Refusing to persist backfilled event into a missing room; it was likely purged")
			continue
		}
		roomNID = roomInfo.RoomNID

		eventTypeNID, err := db.GetOrCreateEventTypeNID(ctx, ev.Type())
		if err != nil {
			logrus.WithError(err).Error("failed to get or create eventType NID")
			continue
		}

		eventStateKeyNID, err := db.GetOrCreateEventStateKeyNID(ctx, ev.StateKey())
		if err != nil {
			logrus.WithError(err).Error("failed to get or create eventStateKey NID")
			continue
		}

		eventNID, _, err = db.StoreEvent(ctx, ev, roomInfo, eventTypeNID, eventStateKeyNID, authNids, false, false)
		if err != nil {
			logrus.WithError(err).WithField("event_id", ev.EventID()).Error("Failed to persist event")
			continue
		}

		storedEvents = append(storedEvents, types.Event{
			EventNID: eventNID,
			PDU:      ev,
		})
	}
	return roomNID, storedEvents
}

// applyBackfilledRedactions applies the redactions that storing the given
// events completed, in either direction: a backfilled redaction of an event
// already stored, or a backfilled event a stored redaction redacts. Whether a
// redaction is allowed depends on the state at the redaction, so this must run
// once the stored events have their state. An event in events that is redacted
// is replaced with its redacted form. It returns the redactions it applied, so
// the caller can redact the copies it already holds.
func applyBackfilledRedactions(
	ctx context.Context, db storage.Database, querier api.QuerySenderIDAPI, roomInfo *types.RoomInfo,
	stored []types.Event, events []gomatrixserverlib.PDU,
) []api.OutputRedactedEvent {
	resolver := state.NewStateResolution(db, roomInfo, querier)
	var redactions []api.OutputRedactedEvent
	for _, ev := range stored {
		redaction, redacted, err := db.MaybeRedactEvent(ctx, roomInfo, ev.EventNID, ev.PDU, &resolver, querier)
		if err != nil {
			logrus.WithError(err).WithField("event_id", ev.EventID()).Error("Failed to redact event")
			continue
		}
		if redaction == nil || redacted == nil {
			continue
		}
		for i := range events {
			if events[i].EventID() == redacted.EventID() {
				events[i] = redacted
				break
			}
		}
		redactions = append(redactions, api.OutputRedactedEvent{
			RedactedEventID: redacted.EventID(),
			RedactedBecause: &types.HeaderedEvent{PDU: redaction},
		})
	}
	return redactions
}
