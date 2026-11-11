package internal

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/getsentry/sentry-go"
	"github.com/matrix-org/util"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"

	asAPI "codefloe.com/pat-s/zendrite/appservice/api"
	fsAPI "codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/internal/caching"
	"codefloe.com/pat-s/zendrite/roomserver/acls"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/internal/input"
	"codefloe.com/pat-s/zendrite/roomserver/internal/perform"
	"codefloe.com/pat-s/zendrite/roomserver/internal/query"
	"codefloe.com/pat-s/zendrite/roomserver/producers"
	"codefloe.com/pat-s/zendrite/roomserver/storage"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/setup/process"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// defaultPurgeWaitTimeout is the maximum time a rejoin attempt will wait for
// an in-flight auto-purge to complete before proceeding.
const defaultPurgeWaitTimeout = 30 * time.Second

// RoomserverInternalAPI is an implementation of api.RoomserverInternalAPI.
type RoomserverInternalAPI struct {
	*input.Inputer
	*query.Queryer
	*perform.Inviter
	*perform.Joiner
	*perform.Peeker
	*perform.InboundPeeker
	*perform.Unpeeker
	*perform.Leaver
	*perform.Publisher
	*perform.Backfiller
	*perform.Forgetter
	*perform.Upgrader
	*perform.Admin
	*perform.Creator
	Knocker                *perform.Knocker
	ProcessContext         *process.ProcessContext
	DB                     storage.Database
	Cfg                    *config.Zendrite
	Cache                  caching.RoomServerCaches
	ServerName             spec.ServerName
	KeyRing                gomatrixserverlib.JSONVerifier
	ServerACLs             *acls.ServerACLs
	fsAPI                  fsAPI.RoomserverFederationAPI
	asAPI                  asAPI.AppServiceInternalAPI
	userAPI                userapi.RoomserverUserAPI
	NATSClient             *nats.Conn
	JetStream              nats.JetStreamContext
	Durable                string
	InputRoomEventTopic    string // JetStream topic for new input room events
	OutputProducer         *producers.RoomEventProducer
	PerspectiveServerNames []spec.ServerName
	enableMetrics          bool
	defaultRoomVersion     gomatrixserverlib.RoomVersion
	PartialStateTracker    *PartialStateTracker
	PurgeTracker           *perform.PurgeTracker
}

func NewRoomserverAPI(
	processContext *process.ProcessContext, zendriteCfg *config.Zendrite, roomserverDB storage.Database,
	js nats.JetStreamContext, nc *nats.Conn, caches caching.RoomServerCaches, enableMetrics bool,
) *RoomserverInternalAPI {
	var perspectiveServerNames []spec.ServerName
	for _, kp := range zendriteCfg.FederationAPI.KeyPerspectives {
		perspectiveServerNames = append(perspectiveServerNames, kp.ServerName)
	}

	serverACLs := acls.NewServerACLs(roomserverDB)
	producer := &producers.RoomEventProducer{
		Topic:     zendriteCfg.Global.JetStream.Prefixed(jetstream.OutputRoomEvent),
		JetStream: js,
		ACLs:      serverACLs,
	}
	a := &RoomserverInternalAPI{
		ProcessContext:         processContext,
		DB:                     roomserverDB,
		Cfg:                    zendriteCfg,
		Cache:                  caches,
		ServerName:             zendriteCfg.Global.ServerName,
		PerspectiveServerNames: perspectiveServerNames,
		InputRoomEventTopic:    zendriteCfg.Global.JetStream.Prefixed(jetstream.InputRoomEvent),
		OutputProducer:         producer,
		JetStream:              js,
		NATSClient:             nc,
		Durable:                zendriteCfg.Global.JetStream.Durable("RoomserverInputConsumer"),
		ServerACLs:             serverACLs,
		enableMetrics:          enableMetrics,
		defaultRoomVersion:     zendriteCfg.RoomServer.DefaultRoomVersion,
		PartialStateTracker:    NewPartialStateTracker(),
		PurgeTracker:           perform.NewPurgeTracker(),
		// perform-er structs + queryer struct get initialized when we have a federation sender to use
	}
	return a
}

// SetFederationInputAPI passes in a federation input API reference so that we can
// avoid the chicken-and-egg problem of both the roomserver input API and the
// federation input API being interdependent.
func (r *RoomserverInternalAPI) SetFederationAPI(fsAPI fsAPI.RoomserverFederationAPI, keyRing *gomatrixserverlib.KeyRing) {
	r.fsAPI = fsAPI
	r.KeyRing = keyRing

	r.Queryer = &query.Queryer{
		DB:                r.DB,
		Cache:             r.Cache,
		IsLocalServerName: r.Cfg.Global.IsLocalServerName,
		ServerACLs:        r.ServerACLs,
		Cfg:               r.Cfg,
		FSAPI:             fsAPI,
	}

	r.Inputer = &input.Inputer{
		Cfg:                 &r.Cfg.RoomServer,
		ProcessContext:      r.ProcessContext,
		DB:                  r.DB,
		InputRoomEventTopic: r.InputRoomEventTopic,
		OutputProducer:      r.OutputProducer,
		JetStream:           r.JetStream,
		NATSClient:          r.NATSClient,
		Durable:             nats.Durable(r.Durable),
		ServerName:          r.ServerName,
		SigningIdentity:     r.SigningIdentityFor,
		FSAPI:               fsAPI,
		RSAPI:               r,
		KeyRing:             keyRing,
		ACLs:                r.ServerACLs,
		Queryer:             r.Queryer,
		EnableMetrics:       r.enableMetrics,
	}
	r.Inviter = &perform.Inviter{
		DB:      r.DB,
		Cfg:     &r.Cfg.RoomServer,
		FSAPI:   r.fsAPI,
		RSAPI:   r,
		Inputer: r.Inputer,
	}
	r.Joiner = &perform.Joiner{
		Cfg:              &r.Cfg.RoomServer,
		DB:               r.DB,
		FSAPI:            r.fsAPI,
		RSAPI:            r,
		Inputer:          r.Inputer,
		Queryer:          r.Queryer,
		PurgeTracker:     r.PurgeTracker,
		PurgeWaitTimeout: defaultPurgeWaitTimeout,
	}
	r.Peeker = &perform.Peeker{
		ServerName: r.ServerName,
		Cfg:        &r.Cfg.RoomServer,
		DB:         r.DB,
		FSAPI:      r.fsAPI,
		Inputer:    r.Inputer,
	}
	r.InboundPeeker = &perform.InboundPeeker{
		DB:      r.DB,
		Inputer: r.Inputer,
	}
	r.Unpeeker = &perform.Unpeeker{
		ServerName: r.ServerName,
		Cfg:        &r.Cfg.RoomServer,
		FSAPI:      r.fsAPI,
		Inputer:    r.Inputer,
	}
	r.Knocker = &perform.Knocker{
		Cfg:     &r.Cfg.RoomServer,
		DB:      r.DB,
		FSAPI:   r.fsAPI,
		RSAPI:   r,
		Inputer: r.Inputer,
	}
	r.Leaver = &perform.Leaver{
		Cfg:     &r.Cfg.RoomServer,
		DB:      r.DB,
		FSAPI:   r.fsAPI,
		RSAPI:   r,
		Inputer: r.Inputer,
	}
	r.Publisher = &perform.Publisher{
		DB: r.DB,
	}
	r.Backfiller = &perform.Backfiller{
		IsLocalServerName: r.Cfg.Global.IsLocalServerName,
		DB:                r.DB,
		FSAPI:             r.fsAPI,
		Querier:           r.Queryer,
		KeyRing:           r.KeyRing,
		// Perspective servers are trusted to not lie about server keys, so we will also
		// prefer these servers when backfilling (assuming they are in the room) rather
		// than trying random servers
		PreferServers: r.PerspectiveServerNames,
	}
	r.Forgetter = &perform.Forgetter{
		DB:    r.DB,
		RSAPI: r,
	}
	r.Upgrader = &perform.Upgrader{
		Cfg:    &r.Cfg.RoomServer,
		URSAPI: r,
	}
	r.Admin = &perform.Admin{
		DB:           r.DB,
		Cfg:          &r.Cfg.RoomServer,
		Inputer:      r.Inputer,
		Queryer:      r.Queryer,
		Leaver:       r.Leaver,
		PurgeTracker: r.PurgeTracker,
	}
	r.Creator = &perform.Creator{
		DB:    r.DB,
		Cfg:   &r.Cfg.RoomServer,
		RSAPI: r,
	}

	if err := r.Start(); err != nil {
		logrus.WithError(err).Panic("failed to start roomserver input API")
	}

	// Store state events that were stored under the wrong state key under
	// their own key first, since the next repairs read the current state.
	// Then check the rejected events again against the repaired state, and
	// repair rooms whose partial state resync finished before resyncs
	// repaired the forward extremities themselves.
	r.ProcessContext.Go(func() {
		r.RepairStateKeys(r.ProcessContext.Context())
		r.RecheckRejectedEvents(r.ProcessContext.Context())
		r.RepairResyncedRooms(r.ProcessContext.Context())
	})

	// When auto-purge is enabled, sweep any rooms that are already eligible
	// at startup (e.g. left over from a previous run, or from a crash
	// mid-purge during the previous shutdown).
	if r.Cfg.RoomServer.AutoPurgeEnabled() {
		r.ProcessContext.Go(func() {
			if _, err := r.RunEmptyRoomsSweep(r.ProcessContext.Context()); err != nil {
				logrus.WithError(err).Warn("startup empty-rooms sweep failed")
			}
		})
	}
}

// RunEmptyRoomsSweep enumerates all rooms eligible for auto-purge under
// the configured mode and schedules an async auto-purge for each. Returns
// the number of rooms scheduled. Safe to call multiple times;
// AutoPurgeRoom coalesces duplicates. Returns 0 for AutoPurgeNever.
func (r *RoomserverInternalAPI) RunEmptyRoomsSweep(ctx context.Context) (int, error) {
	rooms, err := r.DB.PurgeableRooms(ctx, r.Cfg.RoomServer.AutoPurgeMode)
	if err != nil {
		return 0, err
	}
	for _, roomID := range rooms {
		r.AutoPurgeRoom(ctx, roomID, "startup_sweep")
	}
	if len(rooms) > 0 {
		logrus.WithField("count", len(rooms)).Info("startup empty-rooms sweep scheduled purges")
	}
	return len(rooms), nil
}

// ScheduleAutoPurgeIfEligible looks up the roomInfo for the given roomID
// and dispatches to Inputer.ScheduleAutoPurgeIfEmpty which respects the
// configured AutoPurgeMode. This is the entry point used from non-event
// code paths such as /forget where there is no roomInfo on hand.
func (r *RoomserverInternalAPI) ScheduleAutoPurgeIfEligible(ctx context.Context, roomID string) {
	if !r.Cfg.RoomServer.AutoPurgeEnabled() {
		return
	}
	roomInfo, err := r.DB.RoomInfo(ctx, roomID)
	if err != nil {
		logrus.WithError(err).WithField("room_id", roomID).Warn("auto-purge: failed to fetch room info")
		return
	}
	if roomInfo == nil {
		return
	}
	r.ScheduleAutoPurgeIfEmpty(ctx, roomInfo)
}

func (r *RoomserverInternalAPI) SetUserAPI(userAPI userapi.RoomserverUserAPI) {
	r.userAPI = userAPI
	r.Leaver.UserAPI = userAPI
	r.Inputer.UserAPI = userAPI
}

// InviteBlocked implements api.InviteBlockingAPI.
func (r *RoomserverInternalAPI) InviteBlocked(ctx context.Context, invitee spec.UserID, inviter string) (bool, error) {
	if r.userAPI == nil || !r.Cfg.Global.IsLocalServerName(invitee.Domain()) {
		return false, nil
	}
	rule, err := userapi.InviteRuleFor(ctx, r.userAPI, invitee.String(), inviter)
	return rule == userapi.InviteBlock, err
}

func (r *RoomserverInternalAPI) SetAppserviceAPI(asAPI asAPI.AppServiceInternalAPI) {
	r.asAPI = asAPI
}

func (r *RoomserverInternalAPI) DefaultRoomVersion() gomatrixserverlib.RoomVersion {
	return r.defaultRoomVersion
}

// AutoForgetOnLeaveEnabled reports whether the server is configured to
// automatically forget rooms when a local user transitions to leave or
// ban. Used by the /capabilities endpoint to advertise
// m.forget_forced_upon_leave.
func (r *RoomserverInternalAPI) AutoForgetOnLeaveEnabled() bool {
	return r.Cfg.RoomServer.AutoForgetOnLeave
}

func (r *RoomserverInternalAPI) IsKnownRoom(ctx context.Context, roomID spec.RoomID) (bool, error) {
	return r.Inviter.IsKnownRoom(ctx, roomID)
}

func (r *RoomserverInternalAPI) StateQuerier() gomatrixserverlib.StateQuerier {
	return r.Inviter.StateQuerier()
}

func (r *RoomserverInternalAPI) HandleInvite(
	ctx context.Context, inviteEvent *types.HeaderedEvent,
) error {
	if inviteEvent.StateKey() != nil {
		invitee, err := r.QueryUserIDForSender(ctx, inviteEvent.RoomID(), spec.SenderID(*inviteEvent.StateKey()))
		if err == nil && invitee != nil {
			inviter := string(inviteEvent.SenderID())
			if sender, senderErr := r.QueryUserIDForSender(ctx, inviteEvent.RoomID(), inviteEvent.SenderID()); senderErr == nil && sender != nil {
				inviter = sender.String()
			}
			blocked, blockErr := r.InviteBlocked(ctx, *invitee, inviter)
			if blockErr != nil {
				return blockErr
			}
			if blocked {
				return api.ErrInviteBlocked{Invitee: *invitee}
			}
		}
	}
	// Whether the invite will also arrive as a room event. If the room state
	// cannot be read, assume it will not, so the invite is not lost.
	inRoom := false
	if info, infoErr := r.DB.RoomInfo(ctx, inviteEvent.RoomID().String()); infoErr == nil && info != nil && !info.IsStub() {
		inRoom, _ = r.DB.GetLocalServerInRoom(ctx, info.RoomNID)
	}
	outputEvents, err := r.ProcessInviteMembership(ctx, inviteEvent)
	if err != nil {
		return err
	}
	for i := range outputEvents {
		if outputEvents[i].NewInviteEvent != nil {
			outputEvents[i].NewInviteEvent.NotInRoom = !inRoom
		}
	}
	return r.OutputProducer.ProduceRoomEvents(inviteEvent.RoomID().String(), outputEvents)
}

func (r *RoomserverInternalAPI) RetractPendingMembership(
	ctx context.Context, event *types.HeaderedEvent,
) (bool, error) {
	outputEvents, applied, err := r.Leaver.RetractPendingMembership(ctx, event)
	if err != nil || !applied {
		return applied, err
	}
	return true, r.OutputProducer.ProduceRoomEvents(event.RoomID().String(), outputEvents)
}

func (r *RoomserverInternalAPI) PerformCreateRoom(
	ctx context.Context, userID spec.UserID, roomID spec.RoomID, createRequest *api.PerformCreateRoomRequest,
) (string, *util.JSONResponse) {
	return r.Creator.PerformCreateRoom(ctx, userID, roomID, createRequest)
}

func (r *RoomserverInternalAPI) PerformInvite(
	ctx context.Context,
	req *api.PerformInviteRequest,
) error {
	return r.Inviter.PerformInvite(ctx, req)
}

func (r *RoomserverInternalAPI) PerformLeave(
	ctx context.Context,
	req *api.PerformLeaveRequest,
	res *api.PerformLeaveResponse,
) error {
	outputEvents, err := r.Leaver.PerformLeave(ctx, req, res)
	if err != nil {
		sentry.CaptureException(err)
		return err
	}
	if len(outputEvents) == 0 {
		return nil
	}
	return r.OutputProducer.ProduceRoomEvents(req.RoomID, outputEvents)
}

func (r *RoomserverInternalAPI) PerformKnock(
	ctx context.Context,
	req *api.PerformKnockRequest,
) (string, error) {
	roomID, outputEvents, err := r.Knocker.PerformKnock(ctx, req)
	if err != nil {
		return "", err
	}
	if len(outputEvents) > 0 {
		if err = r.OutputProducer.ProduceRoomEvents(roomID, outputEvents); err != nil {
			return "", err
		}
	}
	return roomID, nil
}

func (r *RoomserverInternalAPI) PerformForget(
	ctx context.Context,
	req *api.PerformForgetRequest,
	resp *api.PerformForgetResponse,
) error {
	return r.Forgetter.PerformForget(ctx, req, resp)
}

// GetOrCreateUserRoomPrivateKey gets the user room key for the specified user. If no key exists yet, a new one is created.
func (r *RoomserverInternalAPI) GetOrCreateUserRoomPrivateKey(ctx context.Context, userID spec.UserID, roomID spec.RoomID) (ed25519.PrivateKey, error) {
	key, err := r.DB.SelectUserRoomPrivateKey(ctx, userID, roomID)
	if err != nil {
		return nil, err
	}
	// no key found, create one
	if len(key) == 0 {
		_, key, err = ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}
		key, err = r.DB.InsertUserRoomPrivatePublicKey(ctx, userID, roomID, key)
		if err != nil {
			return nil, err
		}
	}
	return key, nil
}

func (r *RoomserverInternalAPI) StoreUserRoomPublicKey(ctx context.Context, senderID spec.SenderID, userID spec.UserID, roomID spec.RoomID) error {
	pubKeyBytes, err := senderID.RawBytes()
	if err != nil {
		return err
	}
	_, err = r.DB.InsertUserRoomPublicKey(ctx, userID, roomID, ed25519.PublicKey(pubKeyBytes))
	return err
}

func (r *RoomserverInternalAPI) SigningIdentityFor(ctx context.Context, roomID spec.RoomID, senderID spec.UserID) (fclient.SigningIdentity, error) {
	roomVersion, ok := r.Cache.GetRoomVersion(roomID.String())
	if !ok {
		roomInfo, err := r.DB.RoomInfo(ctx, roomID.String())
		if err != nil {
			return fclient.SigningIdentity{}, err
		}
		if roomInfo != nil {
			roomVersion = roomInfo.RoomVersion
		}
	}
	if roomVersion == gomatrixserverlib.RoomVersionPseudoIDs {
		privKey, err := r.GetOrCreateUserRoomPrivateKey(ctx, senderID, roomID)
		if err != nil {
			return fclient.SigningIdentity{}, err
		}
		return fclient.SigningIdentity{
			PrivateKey: privKey,
			KeyID:      "ed25519:1",
			ServerName: spec.ServerName(spec.SenderIDFromPseudoIDKey(privKey)),
		}, nil
	}
	identity, err := r.Cfg.Global.SigningIdentityFor(senderID.Domain())
	if err != nil {
		return fclient.SigningIdentity{}, err
	}
	return *identity, err
}

func (r *RoomserverInternalAPI) AssignRoomNID(ctx context.Context, roomID spec.RoomID, roomVersion gomatrixserverlib.RoomVersion) (roomNID types.RoomNID, err error) {
	return r.DB.AssignRoomNID(ctx, roomID, roomVersion)
}

func (r *RoomserverInternalAPI) InsertReportedEvent(
	ctx context.Context,
	roomID, eventID, reportingUserID, reason string,
	score int64,
) (int64, error) {
	return r.DB.InsertReportedEvent(ctx, roomID, eventID, reportingUserID, reason, score)
}

// InsertReportedRoom stores a room report (MSC4151).
func (r *RoomserverInternalAPI) InsertReportedRoom(ctx context.Context, roomID, reportingUserID, reason string) (int64, error) {
	return r.DB.InsertReportedRoom(ctx, roomID, reportingUserID, reason)
}

// QueryAdminRoomReports lists room reports for the admin API (MSC4151).
func (r *RoomserverInternalAPI) QueryAdminRoomReports(
	ctx context.Context, from, limit uint64, backwards bool, userID, roomID string,
) ([]api.QueryAdminRoomReportsResponse, int64, error) {
	return r.DB.QueryAdminRoomReports(ctx, from, limit, backwards, userID, roomID)
}

// InsertReportedUser stores a user report (MSC4260).
func (r *RoomserverInternalAPI) InsertReportedUser(ctx context.Context, targetUserID, reportingUserID, reason string) (int64, error) {
	return r.DB.InsertReportedUser(ctx, targetUserID, reportingUserID, reason)
}

// QueryAdminUserReports lists user reports for the admin API (MSC4260).
func (r *RoomserverInternalAPI) QueryAdminUserReports(
	ctx context.Context, from, limit uint64, backwards bool, userID, targetUserID string,
) ([]api.QueryAdminUserReportsResponse, int64, error) {
	return r.DB.QueryAdminUserReports(ctx, from, limit, backwards, userID, targetUserID)
}

// MSC3706 Partial State Join methods.

// SetRoomPartialState marks a room as having partial state after a faster join.
func (r *RoomserverInternalAPI) SetRoomPartialState(ctx context.Context, roomNID types.RoomNID, joinEventNID types.EventNID, joinedVia string, serversInRoom []string, deviceListStreamID int64) error {
	return r.DB.SetRoomPartialState(ctx, roomNID, joinEventNID, joinedVia, serversInRoom, deviceListStreamID)
}

// IsRoomPartialState returns true if the room has partial state.
func (r *RoomserverInternalAPI) IsRoomPartialState(ctx context.Context, roomNID types.RoomNID) (bool, error) {
	return r.DB.IsRoomPartialState(ctx, roomNID)
}

// ClearRoomPartialState removes the partial state flag from a room
// Returns the device list stream ID that was stored at join time for device list replay.
func (r *RoomserverInternalAPI) ClearRoomPartialState(ctx context.Context, roomNID types.RoomNID) (int64, error) {
	return r.DB.ClearRoomPartialState(ctx, roomNID)
}

// GetPartialStateServers returns servers known to be in a partial state room.
func (r *RoomserverInternalAPI) GetPartialStateServers(ctx context.Context, roomNID types.RoomNID) ([]string, error) {
	return r.DB.GetPartialStateServers(ctx, roomNID)
}

// GetPartialStateJoinServer returns the server we joined through for a partial state room.
func (r *RoomserverInternalAPI) GetPartialStateJoinServer(ctx context.Context, roomNID types.RoomNID) (string, error) {
	return r.DB.GetPartialStateJoinServer(ctx, roomNID)
}

// GetPartialStateDeviceListStreamID returns the device list stream ID for a partial state room.
func (r *RoomserverInternalAPI) GetPartialStateDeviceListStreamID(ctx context.Context, roomNID types.RoomNID) (int64, error) {
	return r.DB.GetPartialStateDeviceListStreamID(ctx, roomNID)
}

// GetAllPartialStateRooms returns all rooms with partial state.
func (r *RoomserverInternalAPI) GetAllPartialStateRooms(ctx context.Context) ([]types.RoomNID, error) {
	return r.DB.GetAllPartialStateRooms(ctx)
}

// RoomInfoByNID returns room information for the given room NID.
func (r *RoomserverInternalAPI) RoomInfoByNID(ctx context.Context, roomNID types.RoomNID) (*types.RoomInfo, error) {
	return r.DB.RoomInfoByNID(ctx, roomNID)
}

// LatestEventIDs returns the latest event IDs and state snapshot for a room.
func (r *RoomserverInternalAPI) LatestEventIDs(ctx context.Context, roomNID types.RoomNID) ([]string, types.StateSnapshotNID, int64, error) {
	return r.DB.LatestEventIDs(ctx, roomNID)
}

// RoomIDFromNID returns the room ID for a given room NID.
func (r *RoomserverInternalAPI) RoomIDFromNID(ctx context.Context, roomNID types.RoomNID) (string, error) {
	return r.DB.RoomIDFromNID(ctx, roomNID)
}

// GetPartialStateRoomIDs returns the room IDs of all rooms currently in partial state (MSC3706 faster joins).
// This is used by sync to filter rooms that may have incomplete state.
func (r *RoomserverInternalAPI) GetPartialStateRoomIDs(ctx context.Context) ([]string, error) {
	roomNIDs, err := r.DB.GetAllPartialStateRooms(ctx)
	if err != nil {
		return nil, err
	}
	roomIDs := make([]string, 0, len(roomNIDs))
	for _, nid := range roomNIDs {
		roomID, err := r.DB.RoomIDFromNID(ctx, nid)
		if err != nil {
			// Skip rooms we can't look up
			continue
		}
		roomIDs = append(roomIDs, roomID)
	}
	return roomIDs, nil
}

// NotifyUnPartialStated notifies observers that a room has completed its partial state resync.
// This wakes up any callers waiting in AwaitFullState for this room and emits an output
// event to notify downstream components (like syncapi) about the completion.
func (r *RoomserverInternalAPI) NotifyUnPartialStated(roomID string) {
	r.notifyUnPartialStated(roomID, nil, nil)
}

func (r *RoomserverInternalAPI) notifyUnPartialStated(roomID string, deviceListServers, leftUsers []string) {
	// Wake up any callers waiting for full state
	if r.PartialStateTracker != nil {
		r.PartialStateTracker.NotifyUnPartialStated(roomID)
	}

	// Query local joined members to notify downstream components
	ctx := r.ProcessContext.Context()
	membershipsReq := &api.QueryMembershipsForRoomRequest{
		RoomID:     roomID,
		JoinedOnly: true,
		LocalOnly:  true,
	}
	membershipsRes := &api.QueryMembershipsForRoomResponse{}
	if err := r.QueryMembershipsForRoom(ctx, membershipsReq, membershipsRes); err != nil {
		logrus.WithError(err).WithField("room_id", roomID).Error("Failed to query memberships for un-partial-stated room")
		return
	}

	// Extract user IDs from membership events
	var joinedUserIDs []string
	for _, ev := range membershipsRes.JoinEvents {
		if ev.StateKey != nil && *ev.StateKey != "" {
			joinedUserIDs = append(joinedUserIDs, *ev.StateKey)
		}
	}

	// Emit output event to notify downstream components
	outputEvent := api.OutputEvent{
		Type: api.OutputTypeUnPartialStatedRoom,
		UnPartialStatedRoom: &api.OutputUnPartialStatedRoom{
			RoomID:            roomID,
			JoinedUserIDs:     joinedUserIDs,
			DeviceListServers: deviceListServers,
			LeftUsers:         leftUsers,
		},
	}

	if err := r.OutputProducer.ProduceRoomEvents(roomID, []api.OutputEvent{outputEvent}); err != nil {
		logrus.WithError(err).WithField("room_id", roomID).Error("Failed to produce un-partial-stated room event")
		return
	}

	logrus.WithFields(logrus.Fields{
		"room_id":    roomID,
		"user_count": len(joinedUserIDs),
	}).Info("Room completed partial state resync, notified downstream components")
}

// PartialStateResyncBatch implements api.FederationRoomserverAPI.
func (r *RoomserverInternalAPI) PartialStateResyncBatch(ctx context.Context, roomID string, limit int) ([]string, error) {
	roomInfo, err := r.DB.RoomInfo(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if roomInfo == nil {
		return nil, nil
	}
	return r.Inputer.PartialStateResyncBatch(ctx, roomInfo, limit)
}

// ResyncPartialStateEvent implements api.FederationRoomserverAPI.
func (r *RoomserverInternalAPI) ResyncPartialStateEvent(
	ctx context.Context, roomID, eventID string, statesBefore map[string][]string,
) ([]string, error) {
	roomInfo, err := r.DB.RoomInfo(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if roomInfo == nil {
		return nil, fmt.Errorf("room %s does not exist", roomID)
	}
	needState, rejected, err := r.Inputer.ResyncPartialStateEvent(ctx, roomID, roomInfo, eventID, statesBefore)
	if err != nil || rejected == nil {
		return needState, err
	}
	// Tell the sync API about an event that the full state rejects, so that
	// it is no longer served (MSC3902).
	if *rejected {
		if perr := r.OutputProducer.ProduceRoomEvents(roomID, []api.OutputEvent{{
			Type: api.OutputTypeRejectedEvent,
			RejectedEvent: &api.OutputRejectedEvent{
				RoomID:  roomID,
				EventID: eventID,
			},
		}}); perr != nil {
			logrus.WithError(perr).WithField("event_id", eventID).Error("Failed to announce an event rejected after a partial-state resync")
		}
	}
	return nil, nil
}

// CompletePartialStateResync implements api.FederationRoomserverAPI.
func (r *RoomserverInternalAPI) CompletePartialStateResync(ctx context.Context, roomID string) (bool, error) {
	roomInfo, err := r.DB.RoomInfo(ctx, roomID)
	if err != nil {
		return false, err
	}
	if roomInfo == nil {
		return true, nil
	}
	res, err := r.Inputer.CompletePartialStateResync(ctx, roomID, roomInfo)
	if err != nil || !res.Cleared {
		return false, err
	}
	r.notifyUnPartialStated(roomID, res.DeviceListServers, res.LeftUsers) //nolint:contextcheck
	if res.LocalLeftJoin {
		r.ScheduleAutoPurgeIfEmpty(ctx, roomInfo)
	}
	return true, nil
}

// AwaitFullState waits until the room has full state after a partial-state
// join (MSC3902), or the context ends. A room without partial state, or one
// this server does not know, returns at once.
func (r *RoomserverInternalAPI) AwaitFullState(ctx context.Context, roomID string) error {
	return r.PartialStateTracker.AwaitFullState(ctx, roomID, func() (bool, error) {
		roomInfo, err := r.DB.RoomInfo(ctx, roomID)
		if err != nil || roomInfo == nil {
			return false, err
		}
		return r.DB.IsRoomPartialState(ctx, roomInfo.RoomNID)
	})
}

// IsRoomIDPartialState implements api.SyncRoomserverAPI.
func (r *RoomserverInternalAPI) IsRoomIDPartialState(ctx context.Context, roomID string) (bool, error) {
	roomInfo, err := r.DB.RoomInfo(ctx, roomID)
	if err != nil || roomInfo == nil {
		return false, err
	}
	return r.DB.IsRoomPartialState(ctx, roomInfo.RoomNID)
}
