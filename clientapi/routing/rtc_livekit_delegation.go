// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/clientapi/httputil"
	"codefloe.com/pat-s/zendrite/internal/livekit"
	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/setup/process"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// A delegated delayed leave event is restarted when half its delay has
// passed, and the SFU is asked whether its participant is connected every 15
// seconds, or more often for short delays: SFU webhooks are not guaranteed
// to arrive. A participant who has not connected after two minutes, or a
// participant whose SFU has not answered for that long, is given up: the
// first is taken to have left, the second's leave is left to the delay.
// A delegated delayed event must have a delay of at least ten seconds. A
// webhook body may be 1 MiB.
const (
	liveKitPollInterval      = 15 * time.Second
	liveKitMinDelegatedDelay = 10 * time.Second
	liveKitConnectTimeout    = 2 * time.Minute
	liveKitWebhookMaxBytes   = 1 << 20
)

// liveKitDelegateRequest is the body of POST
// /rtc/livekit/delegate_delayed_leave (MSC4195).
type liveKitDelegateRequest struct {
	URL      string `json:"url"`
	RoomID   string `json:"room_id"`
	SlotID   string `json:"slot_id"`
	MemberID string `json:"member_id"`
	DelayID  string `json:"delay_id"`
}

// delegationKey is what MSC4195 allows one delegation for.
type delegationKey struct {
	userID, roomID, slotID, memberID string
}

// delegationSignal is what an SFU webhook says about a participant.
type delegationSignal struct {
	connected bool
	// replaced is a disconnect because the participant connected again.
	replaced bool
}

// delegation keeps one delayed leave event from being sent while its
// participant is connected to the SFU.
type delegation struct {
	key         delegationKey
	delayID     string
	delay       time.Duration
	delayedAt   time.Time
	participant livekit.Participant
	signals     chan delegationSignal
	stop        context.CancelCauseFunc
}

// liveKitDelegations runs the delayed leave events delegated to Zendrite
// (MSC4195). They are kept in memory: after a restart or an HA failover,
// each delayed event is sent when its own delay runs out, as it would be
// without delegation.
type liveKitDelegations struct {
	ctx     context.Context
	issuer  *livekit.Issuer
	delayed *DelayedEvents
	rsAPI   roomserverAPI.ClientRoomserverAPI
	now     func() time.Time
	// pollInterval, connectTimeout and minDelay are liveKitPollInterval,
	// liveKitConnectTimeout and liveKitMinDelegatedDelay, and sendLeave is
	// send, except in tests.
	pollInterval, connectTimeout, minDelay time.Duration
	sendLeave                              func(ctx context.Context, d *delegation)

	mu            sync.Mutex
	byKey         map[delegationKey]*delegation
	byDelayID     map[string]*delegation
	byParticipant map[livekit.Participant]*delegation
}

func newLiveKitDelegations(
	ctx context.Context, issuer *livekit.Issuer, delayed *DelayedEvents, rsAPI roomserverAPI.ClientRoomserverAPI,
) *liveKitDelegations {
	l := &liveKitDelegations{
		ctx: ctx, issuer: issuer, delayed: delayed, rsAPI: rsAPI, now: time.Now,
		pollInterval: liveKitPollInterval, connectTimeout: liveKitConnectTimeout, minDelay: liveKitMinDelegatedDelay,
		byKey:         make(map[delegationKey]*delegation),
		byDelayID:     make(map[string]*delegation),
		byParticipant: make(map[livekit.Participant]*delegation),
	}
	l.sendLeave = l.send
	return l
}

// Delegate implements POST /_matrix/client/v1/rtc/livekit/delegate_delayed_leave
// (MSC4195): Zendrite keeps restarting a delayed leave event of the user
// while their participant is connected to one of its SFUs, and sends it
// once the participant disconnects.
func (l *liveKitDelegations) Delegate(req *http.Request, device *userapi.Device) util.JSONResponse {
	var body liveKitDelegateRequest
	if resErr := httputil.UnmarshalJSONRequest(req, &body); resErr != nil {
		return *resErr
	}
	if body.URL == "" || body.RoomID == "" || body.SlotID == "" || body.MemberID == "" || body.DelayID == "" {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("url, room_id, slot_id, member_id and delay_id are required.")}
	}
	focus := l.issuer.SFU(body.URL)
	if focus == nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("url is not an SFU of this server.")}
	}
	event, err := l.delayed.userAPI.QueryDelayedEvent(req.Context(), body.DelayID)
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("userAPI.QueryDelayedEvent failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	if !ownedBy(event, device) || event.RoomID != body.RoomID || event.Status != userapi.DelayedEventScheduled {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("No such delayed event is scheduled in this room.")}
	}
	if time.Duration(event.DelayMS)*time.Millisecond < l.minDelay {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("The delay of a delegated delayed event must be at least 10 seconds.")}
	}
	d := &delegation{
		key:       delegationKey{device.UserID, body.RoomID, body.SlotID, body.MemberID},
		delayID:   event.DelayID,
		delay:     time.Duration(event.DelayMS) * time.Millisecond,
		delayedAt: event.DelayedSince.Time(),
		participant: livekit.Participant{
			Focus: focus, Room: livekit.RoomName(body.RoomID, body.SlotID),
			Identity: livekit.ParticipantIdentity(device.UserID, body.MemberID),
		},
		signals: make(chan delegationSignal, 1),
	}
	l.start(d) //nolint:contextcheck // a delegation outlives the request
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

// start runs a delegation in place of any earlier one for the same member
// or delayed event, as MSC4195 asks.
func (l *liveKitDelegations) start(d *delegation) {
	ctx, stop := context.WithCancelCause(l.ctx)
	d.stop = stop
	l.mu.Lock()
	for _, earlier := range []*delegation{l.byKey[d.key], l.byDelayID[d.delayID], l.byParticipant[d.participant]} {
		if earlier != nil {
			l.remove(earlier)
		}
	}
	l.byKey[d.key] = d
	l.byDelayID[d.delayID] = d
	l.byParticipant[d.participant] = d
	l.mu.Unlock()
	process.Go(ctx, func() {
		defer func() {
			l.mu.Lock()
			if l.byKey[d.key] == d {
				l.remove(d)
			}
			l.mu.Unlock()
		}()
		l.run(ctx, d)
	})
}

// remove stops a delegation and forgets it. The caller holds l.mu.
func (l *liveKitDelegations) remove(d *delegation) {
	d.stop(context.Canceled)
	if l.byKey[d.key] == d {
		delete(l.byKey, d.key)
	}
	if l.byDelayID[d.delayID] == d {
		delete(l.byDelayID, d.delayID)
	}
	if l.byParticipant[d.participant] == d {
		delete(l.byParticipant, d.participant)
	}
}

// signal passes what an SFU webhook says about a participant to its
// delegation, if there is one. A signal that finds the previous one still
// unread replaces it, since only the latest state matters.
func (l *liveKitDelegations) signal(p livekit.Participant, s delegationSignal) {
	l.mu.Lock()
	d := l.byParticipant[p]
	l.mu.Unlock()
	if d == nil {
		return
	}
	for {
		select {
		case d.signals <- s:
			return
		default:
		}
		select {
		case <-d.signals:
		default:
		}
	}
}

// run restarts the delayed event while the participant is connected or
// still expected, and sends it once the participant has disconnected. It
// returns when the event is sent or no longer scheduled, or when the
// delegation is replaced.
func (l *liveKitDelegations) run(ctx context.Context, d *delegation) {
	log := logrus.WithFields(logrus.Fields{"room_id": d.key.roomID, "delay_id": d.delayID})
	// Restarting when half the delay has passed, and checking at least
	// twice as often, restarts the event at the latest when three quarters
	// of its delay have passed.
	restartEvery := d.delay / 2                                   //nolint:mnd // half the delay
	ticker := time.NewTicker(min(l.pollInterval, restartEvery/2)) //nolint:mnd // twice per restart

	defer ticker.Stop()
	started := l.now()
	lastRestart := d.delayedAt
	connected := false
	var failingSince time.Time
	for {
		if attempt := l.now(); attempt.Sub(lastRestart) >= restartEvery {
			scheduled, err := l.delayed.userAPI.PerformDelayedEventRestart(ctx, d.delayID, spec.AsTimestamp(attempt))
			switch {
			case ctx.Err() != nil:
				return
			case err != nil:
				log.WithError(err).Warn("Failed to restart a delegated delayed leave event")
			case !scheduled:
				// The client canceled or sent it, or it is being sent.
				return
			default:
				lastRestart = attempt
			}
		}
		select {
		case <-ctx.Done():
			return
		case s := <-d.signals:
			if s.connected {
				connected = true
				continue
			}
			if s.replaced {
				continue
			}
			// Webhooks may arrive late and out of order, so the SFU has the
			// last word on a disconnect. If it does not answer, the next
			// poll asks again.
			present, err := l.issuer.Connected(ctx, d.participant)
			if ctx.Err() != nil {
				return
			}
			if err != nil || present {
				connected = connected || present
				continue
			}
		case <-ticker.C:
			present, err := l.issuer.Connected(ctx, d.participant)
			switch {
			case ctx.Err() != nil:
				return
			case err != nil:
				if failingSince.IsZero() {
					failingSince = l.now()
				}
				if l.now().Sub(failingSince) >= l.connectTimeout {
					log.WithError(err).Warn("The LiveKit SFU does not answer; the delegated delayed leave event is left to its delay")
					return
				}
				continue
			case present:
				connected, failingSince = true, time.Time{}
				continue
			case !connected && l.now().Sub(started) < l.connectTimeout:
				failingSince = time.Time{}
				continue
			}
		}
		if ctx.Err() == nil {
			l.sendLeave(ctx, d)
		}
		return
	}
}

// send sends the delayed leave event now, as if its delay had run out. A
// delegation replaced while it sends does not stop the send, which would
// leave the event claimed until the next restart.
func (l *liveKitDelegations) send(ctx context.Context, d *delegation) {
	ctx = context.WithoutCancel(ctx)
	event, err := l.delayed.userAPI.PerformDelayedEventClaim(ctx, d.delayID)
	if err != nil || event == nil {
		if err != nil {
			logrus.WithError(err).WithField("delay_id", d.delayID).Warn("Failed to claim a delegated delayed leave event")
		}
		return
	}
	eventID, resErr := l.delayed.sendEvent(ctx, event)
	if resErr != nil {
		logrus.WithFields(logrus.Fields{"room_id": event.RoomID, "status": resErr.Code}).Warn("Failed to send a delegated delayed leave event")
	}
	l.delayed.finalize(ctx, event, eventID, resErr)
}

// Webhook implements POST /_zendrite/livekit/webhook, where Zendrite's SFUs
// report participants joining and leaving. A participant who joins with a
// token Zendrite issued is removed again if they are no longer joined to the
// Matrix room, as MSC4195 recommends while self-hosted SFUs cannot revoke
// tokens, and the delegated delayed leave events follow their participants.
func (l *liveKitDelegations) Webhook(req *http.Request) util.JSONResponse {
	body, err := io.ReadAll(io.LimitReader(req.Body, liveKitWebhookMaxBytes))
	if err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("The request body could not be read.")}
	}
	event, err := l.issuer.Webhook(req.Header.Get("Authorization"), body)
	if errors.Is(err, livekit.ErrWebhookBody) {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("The webhook body is not a LiveKit event.")}
	}
	if err != nil {
		return util.JSONResponse{Code: http.StatusUnauthorized, JSON: spec.Unknown("The webhook could not be verified.")}
	}
	// SFUs that share an API key cannot be told apart, so the event is
	// about the participant on whichever of them Zendrite knows it on.
	for _, participant := range event.Subjects() {
		switch event.Event {
		case livekit.EventParticipantJoined:
			l.signal(participant, delegationSignal{connected: true})
			l.revalidate(req.Context(), participant)
		case livekit.EventParticipantLeft, livekit.EventParticipantConnectionAborted:
			l.signal(participant, delegationSignal{replaced: event.Replaced()})
		}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

// revalidate removes a participant who joined the SFU with a token Zendrite
// issued but is no longer joined to the Matrix room.
func (l *liveKitDelegations) revalidate(ctx context.Context, p livekit.Participant) {
	roomID, userID, ok := l.issuer.Member(p.Focus, p.Room, p.Identity)
	if !ok {
		return
	}
	log := logrus.WithField("room_id", roomID)
	user, err := spec.NewUserID(userID, true)
	if err != nil {
		return
	}
	var membership roomserverAPI.QueryMembershipForUserResponse
	if err = l.rsAPI.QueryMembershipForUser(ctx, &roomserverAPI.QueryMembershipForUserRequest{
		RoomID: roomID, UserID: *user,
	}, &membership); err != nil {
		log.WithError(err).Warn("Failed to check the membership of a LiveKit participant")
		return
	}
	if membership.IsInRoom && membership.Membership == spec.Join {
		return
	}
	if err = l.issuer.RemoveParticipant(ctx, p); err != nil {
		log.WithError(err).Warn("Failed to remove a LiveKit participant who is not joined to the room")
	}
}
