// Copyright 2025 Jackmaninov
// Copyright 2025 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/internal"
	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/process"
)

const (
	// Initial backoff delay after a resync failed with every server.
	partialStateMinBackoff = time.Second * 5
	// Maximum backoff delay (cap).
	partialStateMaxBackoff = time.Hour * 1
	// Jitter bounds for backoff calculation.
	maxJitterMultiplier = 1.4
	minJitterMultiplier = 0.8
	// How many partial-state events the resync takes from the roomserver at
	// a time.
	partialStateBatchSize = 50
	// How long a /state_ids request of the resync may take: the state of a
	// large room takes a while to serve.
	partialStateStateIDsTimeout = 5 * time.Minute
)

// PartialStateWorker resyncs the full state of rooms joined with partial
// state (MSC3706, MSC3902) in the background, one goroutine per room, as
// Synapse does.
//
// The roomserver records which events have partial state. The resync works
// through them oldest first: it asks another server for the state before the
// prev events this server has no full state for (/state_ids at the join's
// prev events, to begin with), fetches the events it lacks, and has the
// roomserver recalculate and check each event against the full state. Once
// no event with partial state is left, the roomserver calculates the room's
// current state and clears its partial-state flag. Everything it relies on
// is stored, so a resync that is interrupted, by a restart or a failover,
// resumes with the events that are left.
type PartialStateWorker struct {
	process *process.ProcessContext
	rsAPI   roomserverAPI.FederationRoomserverAPI
	fedAPI  *FederationInternalAPI

	mu sync.Mutex
	// active holds the rooms with a running resync. A room queued again
	// while its resync runs is marked to be looked at again when it ends,
	// as a new partial join may have made the room partial again.
	active map[types.RoomNID]bool
	again  map[types.RoomNID]bool
}

// NewPartialStateWorker creates a new partial state worker.
func NewPartialStateWorker(
	processCtx *process.ProcessContext,
	rsAPI roomserverAPI.FederationRoomserverAPI,
	fedAPI *FederationInternalAPI,
) *PartialStateWorker {
	return &PartialStateWorker{
		process: processCtx,
		rsAPI:   rsAPI,
		fedAPI:  fedAPI,
		active:  make(map[types.RoomNID]bool),
		again:   make(map[types.RoomNID]bool),
	}
}

// backoffDuration calculates the backoff duration for a given retry count using
// exponential backoff with jitter, similar to the federation queue statistics.
func (w *PartialStateWorker) backoffDuration(retryCount uint32) time.Duration {
	// Add jitter to minimize thundering herd effects
	jitter := rand.Float64()*(maxJitterMultiplier-minJitterMultiplier) + minJitterMultiplier

	// Exponential backoff: minBackoff * 2^retryCount, capped at maxBackoff
	backoff := float64(partialStateMinBackoff) * math.Pow(2, float64(retryCount)) * jitter //nolint:mnd

	duration := time.Duration(backoff)
	if duration > partialStateMaxBackoff || duration <= 0 {
		duration = partialStateMaxBackoff
	}
	return duration
}

// Start resumes the resync of every room that is still in partial state.
func (w *PartialStateWorker) Start() error {
	// Skip if rsAPI is not available (e.g., in tests)
	if w.rsAPI == nil {
		return nil
	}
	roomNIDs, err := w.rsAPI.GetAllPartialStateRooms(w.process.Context())
	if err != nil {
		logrus.WithError(err).Error("Failed to load partial state rooms on startup")
		return err
	}
	if len(roomNIDs) > 0 {
		logrus.WithField("count", len(roomNIDs)).Info("Resuming the partial state resync of rooms")
		// Stagger many rooms so as not to start every resync at once.
		step := 100 * time.Millisecond   //nolint:mnd
		if n := len(roomNIDs); n > 600 { //nolint:mnd
			step = time.Minute / time.Duration(n)
		}
		process.Stagger(w.process.Context(), roomNIDs, 0, step, w.QueueRoom)
	}
	return nil
}

// QueueRoom starts the resync of a room, unless one is running already.
func (w *PartialStateWorker) QueueRoom(roomNID types.RoomNID) {
	if w.process.Context().Err() != nil {
		return
	}
	w.mu.Lock()
	if w.active[roomNID] {
		w.again[roomNID] = true
		w.mu.Unlock()
		return
	}
	w.active[roomNID] = true
	w.mu.Unlock()
	w.process.Go(func() { w.run(roomNID) })
}

// run resyncs a room until it has full state, it is gone, or the server shuts
// down, retrying with backoff while no server can give the state.
func (w *PartialStateWorker) run(roomNID types.RoomNID) {
	defer func() {
		w.mu.Lock()
		again := w.again[roomNID]
		delete(w.again, roomNID)
		delete(w.active, roomNID)
		w.mu.Unlock()
		if again {
			w.QueueRoom(roomNID)
		}
	}()
	ctx := w.process.Context()
	for attempt := uint32(0); ; attempt++ {
		err := w.processRoom(ctx, roomNID)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		backoff := w.backoffDuration(attempt)
		logrus.WithError(err).WithFields(logrus.Fields{
			"room_nid": roomNID,
			"attempt":  attempt + 1,
			"retry_in": backoff,
		}).Warn("Partial state resync failed, will retry")
		if !process.Sleep(ctx, backoff) {
			return
		}
	}
}

// processRoom resyncs the state of a room with partial state.
func (w *PartialStateWorker) processRoom(ctx context.Context, roomNID types.RoomNID) error {
	trace, ctx := internal.StartTask(ctx, "PartialStateWorker.processRoom")
	defer trace.EndTask()
	trace.SetTag("room_nid", roomNID)

	partial, err := w.rsAPI.IsRoomPartialState(ctx, roomNID)
	if err != nil {
		return err
	}
	if !partial {
		return nil
	}
	roomID, err := w.rsAPI.RoomIDFromNID(ctx, roomNID)
	if err != nil {
		return fmt.Errorf("RoomIDFromNID: %w", err)
	}
	roomInfo, err := w.rsAPI.RoomInfoByNID(ctx, roomNID)
	if err != nil {
		return fmt.Errorf("RoomInfoByNID: %w", err)
	}
	if roomInfo == nil {
		return fmt.Errorf("room %s is gone", roomID)
	}
	joinServer, err := w.rsAPI.GetPartialStateJoinServer(ctx, roomNID)
	if err != nil {
		return err
	}
	servers, err := w.rsAPI.GetPartialStateServers(ctx, roomNID)
	if err != nil {
		return err
	}
	logger := logrus.WithFields(logrus.Fields{"room_id": roomID, "room_nid": roomNID})
	destinations := w.buildServerList(joinServer, servers)
	if len(destinations) == 0 {
		return fmt.Errorf("no servers to resync the state of %s from", roomID)
	}
	trace.SetTag("room_id", roomID)
	logger.WithField("server", destinations[0]).Info("Resyncing the state of a room joined with partial state")

	started := time.Now()
	current := 0
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		batch, berr := w.rsAPI.PartialStateResyncBatch(ctx, roomID, partialStateBatchSize)
		if berr != nil {
			return fmt.Errorf("PartialStateResyncBatch: %w", berr)
		}
		if len(batch) == 0 {
			cleared, cerr := w.rsAPI.CompletePartialStateResync(ctx, roomID)
			if cerr != nil {
				return fmt.Errorf("CompletePartialStateResync: %w", cerr)
			}
			if !cleared {
				// An event with partial state arrived meanwhile.
				continue
			}
			logger.WithField("duration", time.Since(started)).Info("Partial state resync complete")
			return nil
		}
		for _, eventID := range batch {
			if current, err = w.resyncEvent(ctx, logger, roomID, roomInfo.RoomVersion, eventID, destinations, current); err != nil {
				if errors.Is(err, roomserverAPI.ErrPartialStatePrevEvents) {
					// Its prev event became partial meanwhile: take a new batch.
					break
				}
				return err
			}
		}
	}
}

// resyncEvent has the roomserver recalculate the state of one partial-state
// event, fetching the state before the prev events it has no full state for
// from the servers in turn, starting with the current one. It returns the
// server to start with next time.
func (w *PartialStateWorker) resyncEvent(
	ctx context.Context, logger *logrus.Entry, roomID string, roomVersion gomatrixserverlib.RoomVersion,
	eventID string, destinations []spec.ServerName, current int,
) (int, error) {
	statesBefore := map[string][]string{}
	failures := 0
	for {
		needState, err := w.rsAPI.ResyncPartialStateEvent(ctx, roomID, eventID, statesBefore)
		if err != nil {
			return current, err
		}
		if len(needState) == 0 {
			return current, nil
		}
		for _, prevID := range needState {
			server := destinations[current%len(destinations)]
			stateIDs, ferr := w.fetchStateBefore(ctx, logger, roomID, roomVersion, prevID, server, destinations)
			if ferr != nil {
				logger.WithError(ferr).WithFields(logrus.Fields{
					"server":   server,
					"event_id": prevID,
				}).Warn("Failed to fetch the state before an event for a partial state resync")
				failures++
				if failures >= len(destinations) {
					return current, fmt.Errorf("no server gave the state before %s: %w", prevID, ferr)
				}
				current++
				break
			}
			statesBefore[prevID] = stateIDs
		}
	}
}

// fetchStateBefore fetches the IDs of the state before an event from a
// server with /state_ids, and stores the event and every state and auth event
// this server lacks as outliers, fetching them with /event.
func (w *PartialStateWorker) fetchStateBefore(
	ctx context.Context, logger *logrus.Entry, roomID string, roomVersion gomatrixserverlib.RoomVersion,
	eventID string, server spec.ServerName, destinations []spec.ServerName,
) ([]string, error) {
	trace, ctx := internal.StartRegion(ctx, "fetchStateBefore")
	defer trace.EndRegion()

	origin := w.fedAPI.cfg.Matrix.ServerName
	reqCtx, cancel := context.WithTimeout(ctx, partialStateStateIDsTimeout)
	defer cancel()
	stateIDs, err := w.fedAPI.LookupStateIDsBypassBackoff(reqCtx, origin, server, roomID, eventID)
	if err != nil {
		return nil, fmt.Errorf("LookupStateIDs: %w", err)
	}
	wantIDs := stateIDs.GetStateEventIDs()
	if len(wantIDs) == 0 {
		return nil, fmt.Errorf("%s returned no state before %s", server, eventID)
	}
	allIDs := make(map[string]bool, len(wantIDs)+len(stateIDs.GetAuthEventIDs())+1)
	for _, id := range wantIDs {
		allIDs[id] = true
	}
	for _, id := range stateIDs.GetAuthEventIDs() {
		allIDs[id] = true
	}
	allIDs[eventID] = true
	logger.WithFields(logrus.Fields{
		"server":       server,
		"event_id":     eventID,
		"state_events": len(wantIDs),
		"auth_events":  len(stateIDs.GetAuthEventIDs()),
	}).Info("Fetched state IDs for a partial state resync")

	if missing := w.findMissingEvents(ctx, logger, roomID, allIDs, false); len(missing) > 0 {
		if err = w.fetchMissingEventsParallel(ctx, logger, origin, roomVersion, missing, server, destinations); err != nil {
			return nil, err
		}
	}

	want := make(map[string]bool, len(wantIDs)+1)
	for _, id := range wantIDs {
		want[id] = true
	}
	want[eventID] = true
	if stillMissing := w.findMissingEvents(ctx, logger, roomID, want, false); len(stillMissing) > 0 {
		return nil, fmt.Errorf("%d of %d state events before %s could not be fetched", len(stillMissing), len(want), eventID)
	}

	// A state without the room's create event cannot be right.
	var res roomserverAPI.QueryEventsByIDResponse
	if err = w.rsAPI.QueryEventsByID(ctx, &roomserverAPI.QueryEventsByIDRequest{
		RoomID:   roomID,
		EventIDs: wantIDs,
	}, &res); err != nil {
		return nil, fmt.Errorf("QueryEventsByID: %w", err)
	}
	hasCreate := false
	for _, ev := range res.Events {
		if ev.Type() == spec.MRoomCreate && ev.StateKeyEquals("") {
			hasCreate = true
			break
		}
	}
	if !hasCreate {
		return nil, fmt.Errorf("the state %s returned before %s has no create event", server, eventID)
	}
	return wantIDs, nil
}

// buildServerList returns the servers to resync from: the server the room was
// joined through first, as Synapse does, then the other servers that were in
// the room. Neither the resync's requests nor their failures count towards
// the servers' backoff, so they are all tried.
func (w *PartialStateWorker) buildServerList(joinServer string, servers []string) []spec.ServerName {
	var result []spec.ServerName
	own := spec.ServerName("")
	if w.fedAPI != nil && w.fedAPI.cfg != nil {
		own = w.fedAPI.cfg.Matrix.ServerName
	}
	if joinServer != "" {
		result = append(result, spec.ServerName(joinServer))
	}
	for _, serverStr := range servers {
		server := spec.ServerName(serverStr)
		if serverStr == joinServer || server == own {
			continue
		}
		if w.fedAPI != nil && w.fedAPI.cfg != nil && w.fedAPI.cfg.Matrix.IsLocalServerName(server) {
			continue
		}
		result = append(result, server)
	}
	return result
}

// findMissingEvents returns which of the wanted events are not stored. When
// excludeRejected is true, rejected events count as missing.
func (w *PartialStateWorker) findMissingEvents(
	ctx context.Context,
	logger *logrus.Entry,
	roomID string,
	wantIDs map[string]bool,
	excludeRejected bool,
) []string {
	idList := make([]string, 0, len(wantIDs))
	for id := range wantIDs {
		idList = append(idList, id)
	}

	var res roomserverAPI.QueryEventsByIDResponse
	if err := w.rsAPI.QueryEventsByID(ctx, &roomserverAPI.QueryEventsByIDRequest{
		RoomID:          roomID,
		EventIDs:        idList,
		ExcludeRejected: excludeRejected,
	}, &res); err != nil {
		logger.WithError(err).Warn("Failed to query existing events, treating all as missing")
		return idList
	}

	known := make(map[string]bool, len(res.Events))
	for _, ev := range res.Events {
		known[ev.EventID()] = true
	}

	var missing []string
	for id := range wantIDs {
		if !known[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

// fetchMissingEventsParallel fetches missing events with /event, from the
// given server first, and stores them as outliers, each after its auth events.
func (w *PartialStateWorker) fetchMissingEventsParallel(
	ctx context.Context,
	logger *logrus.Entry,
	origin spec.ServerName,
	roomVersion gomatrixserverlib.RoomVersion,
	missing []string,
	primaryServer spec.ServerName,
	allServers []spec.ServerName,
) error {
	const concurrentRequests = 8

	verImpl, err := gomatrixserverlib.GetRoomVersion(roomVersion)
	if err != nil {
		return fmt.Errorf("GetRoomVersion: %w", err)
	}
	logger.WithField("missing", len(missing)).Info("Fetching missing events for a partial state resync")

	workers := min(concurrentRequests, len(missing))
	pending := make(chan string, len(missing))
	for _, id := range missing {
		pending <- id
	}
	close(pending)

	var fetched []gomatrixserverlib.PDU
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for eventID := range pending {
				if ev := w.fetchSingleEvent(ctx, origin, eventID, verImpl, primaryServer, allServers); ev != nil {
					mu.Lock()
					fetched = append(fetched, ev)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if err = ctx.Err(); err != nil {
		return err
	}

	// Store each event after its auth events, so that their checks find them.
	fetched = gomatrixserverlib.ReverseTopologicalOrdering(fetched, gomatrixserverlib.TopologicalOrderByAuthEvents)
	const batchSize = 500
	for start := 0; start < len(fetched); start += batchSize {
		end := min(start+batchSize, len(fetched))
		inputs := make([]roomserverAPI.InputRoomEvent, 0, end-start)
		for _, ev := range fetched[start:end] {
			inputs = append(inputs, roomserverAPI.InputRoomEvent{
				Kind:   roomserverAPI.KindOutlier,
				Event:  &types.HeaderedEvent{PDU: ev},
				Origin: primaryServer,
			})
		}
		var ires roomserverAPI.InputRoomEventsResponse
		w.rsAPI.InputRoomEvents(ctx, &roomserverAPI.InputRoomEventsRequest{InputRoomEvents: inputs}, &ires)
		if ires.ErrMsg != "" {
			logger.WithField("error", ires.ErrMsg).Warn("Failed to store fetched events for a partial state resync")
		}
	}
	return nil
}

// fetchSingleEvent fetches an event from the primary server, then from up to
// four others.
func (w *PartialStateWorker) fetchSingleEvent(
	ctx context.Context,
	origin spec.ServerName,
	eventID string,
	verImpl gomatrixserverlib.IRoomVersion,
	primaryServer spec.ServerName,
	allServers []spec.ServerName,
) gomatrixserverlib.PDU {
	ev, err := w.getEventFromServer(ctx, origin, primaryServer, eventID, verImpl)
	if err == nil {
		return ev
	}
	const maxFallbacks = 4
	tried := 0
	for _, server := range allServers {
		if server == primaryServer {
			continue
		}
		if tried >= maxFallbacks {
			break
		}
		tried++
		if ev, err = w.getEventFromServer(ctx, origin, server, eventID, verImpl); err == nil {
			return ev
		}
	}
	return nil
}

func (w *PartialStateWorker) getEventFromServer(
	ctx context.Context,
	origin, serverName spec.ServerName,
	eventID string,
	verImpl gomatrixserverlib.IRoomVersion,
) (gomatrixserverlib.PDU, error) {
	reqctx, cancel := context.WithTimeout(ctx, 30*time.Second) //nolint:mnd
	defer cancel()

	txn, err := w.fedAPI.GetEventBypassBackoff(reqctx, origin, serverName, eventID)
	if err != nil || len(txn.PDUs) == 0 {
		return nil, fmt.Errorf("GetEvent %s from %s: %w", eventID, serverName, err)
	}
	ev, err := verImpl.NewEventFromUntrustedJSON(txn.PDUs[0])
	if err != nil {
		return nil, fmt.Errorf("parse event %s from %s: %w", eventID, serverName, err)
	}
	if ev.EventID() != eventID {
		return nil, fmt.Errorf("%s returned event %s for %s", serverName, ev.EventID(), eventID)
	}
	return ev, nil
}
