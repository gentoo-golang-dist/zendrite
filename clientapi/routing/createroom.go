// Copyright 2024 New Vector Ltd.
// Copyright 2017 Vector Creations Ltd
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/nats-io/nats.go"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	appserviceAPI "codefloe.com/pat-s/zendrite/appservice/api"
	"codefloe.com/pat-s/zendrite/clientapi/httputil"
	roomserverAPI "codefloe.com/pat-s/zendrite/roomserver/api"
	roomserverVersion "codefloe.com/pat-s/zendrite/roomserver/version"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// https://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-createroom
type createRoomRequest struct {
	Invite                    []string                           `json:"invite"`
	Name                      string                             `json:"name"`
	Visibility                string                             `json:"visibility"`
	Topic                     string                             `json:"topic"`
	Preset                    string                             `json:"preset"`
	CreationContent           json.RawMessage                    `json:"creation_content"`
	InitialState              []gomatrixserverlib.FledglingEvent `json:"initial_state"`
	RoomAliasName             string                             `json:"room_alias_name"`
	RoomVersion               gomatrixserverlib.RoomVersion      `json:"room_version"`
	PowerLevelContentOverride json.RawMessage                    `json:"power_level_content_override"`
	IsDirect                  bool                               `json:"is_direct"`
	// InviteReason is MSC4491's reason for the invites in Invite. It is raw
	// JSON because, like Synapse, a value that is not a string is ignored
	// rather than refused.
	InviteReason json.RawMessage `json:"uk.timedout.msc4491.invite_reason"`
}

// maxInviteReasonBytes bounds an MSC4491 invite reason. An event can be no
// larger than 65536 bytes, so a longer reason could never be sent; refusing
// it up front keeps the room from being created without its invites.
const maxInviteReasonBytes = 65536

// inviteReason returns the MSC4491 reason to put on the invites createRoom
// sends, or "" when the request has none. As in Synapse, a reason that is
// not a JSON string is ignored.
func (r createRoomRequest) inviteReason() string {
	var reason string
	if len(r.InviteReason) == 0 || json.Unmarshal(r.InviteReason, &reason) != nil {
		return ""
	}
	return reason
}

func (r createRoomRequest) Validate() *util.JSONResponse {
	whitespace := "\t\n\x0b\x0c\r " // https://docs.python.org/2/library/string.html#string.whitespace
	// https://github.com/matrix-org/synapse/blob/v0.19.2/synapse/handlers/room.py#L81
	// Synapse doesn't check for ':' but we will else it will break parsers badly which split things into 2 segments.
	if strings.ContainsAny(r.RoomAliasName, whitespace+":") {
		return &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("room_alias_name cannot contain whitespace or ':'"),
		}
	}
	for _, userID := range r.Invite {
		if _, err := spec.NewUserID(userID, true); err != nil {
			return &util.JSONResponse{
				Code: http.StatusBadRequest,
				JSON: spec.BadJSON("user id must be in the form @localpart:domain"),
			}
		}
	}
	if len(r.inviteReason()) > maxInviteReasonBytes {
		return &util.JSONResponse{
			Code: http.StatusRequestEntityTooLarge,
			JSON: spec.TooLarge("uk.timedout.msc4491.invite_reason is too long"),
		}
	}
	switch r.Preset {
	case spec.PresetPrivateChat, spec.PresetTrustedPrivateChat, spec.PresetPublicChat, "":
	default:
		return &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("preset must be any of 'private_chat', 'trusted_private_chat', 'public_chat'"),
		}
	}

	// Validate creation_content fields defined in the spec by marshaling the
	// creation_content map into bytes and then unmarshaling the bytes into
	// eventutil.CreateContent.

	creationContentBytes, err := json.Marshal(r.CreationContent)
	if err != nil {
		return &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("malformed creation_content"),
		}
	}

	var CreationContent gomatrixserverlib.CreateContent
	err = json.Unmarshal(creationContentBytes, &CreationContent)
	if err != nil {
		return &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("malformed creation_content"),
		}
	}

	return nil
}

// https://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-createroom
type createRoomResponse struct {
	RoomID    string `json:"room_id"`
	RoomAlias string `json:"room_alias,omitempty"` // in synapse not spec
}

// CreateRoom implements /createRoom.
func CreateRoom(
	req *http.Request, device *api.Device,
	cfg *config.ClientAPI,
	profileAPI api.ClientUserAPI, rsAPI roomserverAPI.ClientRoomserverAPI,
	asAPI appserviceAPI.AppServiceInternalAPI,
	natsClient *nats.Conn,
) util.JSONResponse {
	var createRequest createRoomRequest
	resErr := httputil.UnmarshalJSONRequest(req, &createRequest)
	if resErr != nil {
		return *resErr
	}
	if resErr = createRequest.Validate(); resErr != nil {
		return *resErr
	}
	evTime, err := httputil.ParseTSParam(req, device)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam(err.Error()),
		}
	}
	res := createRoom(req.Context(), createRequest, device, cfg, profileAPI, rsAPI, asAPI, evTime)
	if created, ok := res.JSON.(createRoomResponse); ok && res.Code == http.StatusOK {
		// The sync API receives the room's events from the roomserver after
		// this request returns. Wait for them, so that a sync straight after
		// createRoom returns a token that includes the whole room creation.
		waitForSyncedRoom(
			req.Context(), rsAPI, natsClient,
			cfg.Matrix.JetStream.Prefixed(jetstream.RequestSyncedEvent), created.RoomID,
		)
	}
	return res
}

// syncedRoomWait bounds how long createRoom and joins wait for the sync API.
const syncedRoomWait = 5 * time.Second

// waitForSyncedRoom waits until the sync API reports that a sync token taken
// now includes the room's latest events. It gives up quietly after
// syncedRoomWait, as the events reach the sync API either way.
func waitForSyncedRoom(
	ctx context.Context, rsAPI roomserverAPI.ClientRoomserverAPI, natsClient *nats.Conn, topic, roomID string,
) {
	if natsClient == nil {
		return
	}
	latest := roomserverAPI.QueryLatestEventsAndStateResponse{}
	if err := rsAPI.QueryLatestEventsAndState(ctx, &roomserverAPI.QueryLatestEventsAndStateRequest{
		RoomID:       roomID,
		StateToFetch: []gomatrixserverlib.StateKeyTuple{{EventType: spec.MRoomCreate, StateKey: ""}},
	}, &latest); err != nil {
		util.GetLogger(ctx).WithError(err).Warn("Unable to query the room's latest events to wait for the sync API")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, syncedRoomWait)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for _, eventID := range latest.LatestEvents {
		for {
			msg := nats.NewMsg(topic)
			msg.Header.Set(jetstream.RoomID, roomID)
			msg.Header.Set(jetstream.EventID, eventID)
			res, err := natsClient.RequestMsgWithContext(ctx, msg)
			if err != nil {
				return
			}
			if res.Header.Get(jetstream.Synced) == "true" {
				break
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
}

// createRoom implements /createRoom.
func createRoom(
	ctx context.Context,
	createRequest createRoomRequest, device *api.Device,
	cfg *config.ClientAPI,
	profileAPI api.ClientUserAPI, rsAPI roomserverAPI.ClientRoomserverAPI,
	asAPI appserviceAPI.AppServiceInternalAPI,
	evTime time.Time,
) util.JSONResponse {
	userID, err := spec.NewUserID(device.UserID, true)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("invalid userID")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	if !cfg.Matrix.IsLocalServerName(userID.Domain()) {
		return util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.Forbidden(fmt.Sprintf("User domain %q not configured locally", userID.Domain())),
		}
	}

	logger := util.GetLogger(ctx)

	// TODO: Check room ID doesn't clash with an existing one, and we
	//       probably shouldn't be using pseudo-random strings, maybe GUIDs?
	roomID, err := spec.NewRoomID(fmt.Sprintf("!%s:%s", util.RandomString(16), userID.Domain())) //nolint:mnd
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("invalid roomID")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	// Clobber keys: creator, room_version

	roomVersion := rsAPI.DefaultRoomVersion()
	if createRequest.RoomVersion != "" {
		candidateVersion := createRequest.RoomVersion
		_, roomVersionError := roomserverVersion.SupportedRoomVersion(candidateVersion)
		if roomVersionError != nil {
			return util.JSONResponse{
				Code: http.StatusBadRequest,
				JSON: spec.UnsupportedRoomVersion(roomVersionError.Error()),
			}
		}
		roomVersion = candidateVersion
	}

	profile, err := appserviceAPI.RetrieveUserProfile(ctx, userID.String(), asAPI, profileAPI)
	if err != nil {
		util.GetLogger(ctx).WithError(err).Error("appserviceAPI.RetrieveUserProfile failed")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	userDisplayName := profile.DisplayName
	userAvatarURL := profile.AvatarURL

	keyID := cfg.Matrix.KeyID
	privateKey := cfg.Matrix.PrivateKey

	verImpl := gomatrixserverlib.MustGetRoomVersion(roomVersion)

	var createEventJSON json.RawMessage
	if verImpl.DomainlessRoomIDs() {
		// make the create event up-front so the roomserver can calculate the room NID to store.
		var additionalCreators []string
		if createRequest.Preset == spec.PresetTrustedPrivateChat {
			additionalCreators = createRequest.Invite
		}
		createContent, err := roomserverAPI.GenerateCreateContent(ctx, roomVersion, userID.String(), createRequest.CreationContent, additionalCreators)
		if err != nil {
			util.GetLogger(ctx).WithError(err).Error("GenerateCreateContent failed")
			return util.JSONResponse{
				Code: http.StatusBadRequest,
				JSON: spec.BadJSON("invalid create content"),
			}
		}
		authEvents, _ := gomatrixserverlib.NewAuthEvents(nil)
		identity, err := cfg.Matrix.SigningIdentityFor(userID.Domain())
		if err != nil {
			util.GetLogger(ctx).WithError(err).Error("Failed to get signing identity")
			return util.JSONResponse{
				Code: http.StatusInternalServerError,
				JSON: spec.InternalServerError{},
			}
		}
		createEvent, jsonErr := roomserverAPI.GeneratePDU(
			ctx, gomatrixserverlib.MustGetRoomVersion(roomVersion),
			gomatrixserverlib.FledglingEvent{
				Type:    spec.MRoomCreate,
				Content: createContent,
			},
			authEvents, 1, "", identity, evTime, userID.String(), "", rsAPI,
		)
		if jsonErr != nil {
			util.GetLogger(ctx).WithError(err).Error("Failed to make the create event")
			return *jsonErr
		}
		createEventJSON = createEvent.JSON()
		r := createEvent.RoomID()
		roomID = &r

		// A client that asks for this room version knows that creators cannot
		// be listed in the power levels. Clients which leave the version to the
		// server may predate that rule, so the roomserver drops creators for
		// them instead.
		if createRequest.RoomVersion != "" && verImpl.PrivilegedCreators() {
			overrideUsers := gjson.GetBytes(createRequest.PowerLevelContentOverride, "users")
			for _, creator := range gomatrixserverlib.CreatorsFromCreateEvent(createEvent) {
				if overrideUsers.Get(gjson.Escape(creator)).Exists() {
					return util.JSONResponse{
						Code: http.StatusBadRequest,
						JSON: spec.InvalidParam("power_level_content_override must not contain room creators in this room version"),
					}
				}
			}
		}
	}

	req := roomserverAPI.PerformCreateRoomRequest{
		InvitedUsers:              createRequest.Invite,
		RoomName:                  createRequest.Name,
		Visibility:                createRequest.Visibility,
		Topic:                     createRequest.Topic,
		StatePreset:               createRequest.Preset,
		CreationContent:           createRequest.CreationContent,
		CreateEvent:               createEventJSON,
		InitialState:              createRequest.InitialState,
		RoomAliasName:             createRequest.RoomAliasName,
		RoomVersion:               roomVersion,
		PowerLevelContentOverride: createRequest.PowerLevelContentOverride,
		IsDirect:                  createRequest.IsDirect,
		InviteReason:              createRequest.inviteReason(),

		UserDisplayName: userDisplayName,
		UserAvatarURL:   userAvatarURL,
		KeyID:           keyID,
		PrivateKey:      privateKey,
		EventTime:       evTime,
	}

	logger.WithFields(log.Fields{
		"userID":      userID.String(),
		"roomID":      roomID.String(),
		"roomVersion": roomVersion,
	}).Info("Creating new room")

	roomAlias, createRes := rsAPI.PerformCreateRoom(ctx, *userID, *roomID, &req)
	if createRes != nil {
		return *createRes
	}

	response := createRoomResponse{
		RoomID:    roomID.String(),
		RoomAlias: roomAlias,
	}

	return util.JSONResponse{
		Code: 200, //nolint:mnd
		JSON: response,
	}
}
