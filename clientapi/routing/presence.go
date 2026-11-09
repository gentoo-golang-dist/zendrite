// Copyright 2024 New Vector Ltd.
// Copyright 2022 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
	"github.com/nats-io/nats.go"
	log "github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/clientapi/httputil"
	"codefloe.com/pat-s/zendrite/clientapi/producers"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

type presenceReq struct {
	Presence  string  `json:"presence"`
	StatusMsg *string `json:"status_msg,omitempty"`
}

func SetPresence(
	req *http.Request,
	cfg *config.ClientAPI,
	device *api.Device,
	producer *producers.SyncAPIProducer,
	natsClient *nats.Conn,
	userID string,
) util.JSONResponse {
	if !cfg.Matrix.Presence.EnableOutbound {
		return util.JSONResponse{
			Code: http.StatusOK,
			JSON: struct{}{},
		}
	}
	if device.UserID != userID {
		return util.JSONResponse{
			Code: http.StatusForbidden,
			JSON: spec.Forbidden("Unable to set presence for other user."),
		}
	}
	var presence presenceReq
	parseErr := httputil.UnmarshalJSONRequest(req, &presence)
	if parseErr != nil {
		return *parseErr
	}

	presenceStatus, ok := types.PresenceFromString(presence.Presence)
	if !ok {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.InvalidParam(fmt.Sprintf("Unknown presence '%s'.", presence.Presence)),
		}
	}
	err := producer.SendPresence(req.Context(), userID, presenceStatus, presence.StatusMsg)
	if err != nil {
		log.WithError(err).Errorf("failed to update presence")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	// The sync API stores the update when it consumes it, which can be after
	// this request returns. Wait for it, so that a GET straight after the PUT
	// sees the new presence.
	waitForPresence(
		req.Context(), natsClient, cfg.Matrix.JetStream.Prefixed(jetstream.RequestPresence),
		userID, presenceStatus.String(), presence.StatusMsg,
	)

	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct{}{},
	}
}

// presenceWait bounds how long SetPresence waits for the sync API to store the
// presence it sets.
const presenceWait = 2 * time.Second

// waitForPresence waits until the sync API reports the given presence and,
// if one is given, status message for the user. It gives up quietly after
// presenceWait, as the update is published either way.
func waitForPresence(
	ctx context.Context, natsClient *nats.Conn, topic, userID, presence string, statusMsg *string,
) {
	if natsClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, presenceWait)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		msg := nats.NewMsg(topic)
		msg.Header.Set(jetstream.UserID, userID)
		res, err := natsClient.RequestMsgWithContext(ctx, msg)
		if err != nil {
			return
		}
		if res.Header.Get("error") == "" && res.Header.Get("presence") == presence &&
			(statusMsg == nil || res.Header.Get("status_msg") == *statusMsg) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func GetPresence(
	req *http.Request,
	device *api.Device,
	natsClient *nats.Conn,
	presenceTopic string,
	userID string,
) util.JSONResponse {
	msg := nats.NewMsg(presenceTopic)
	msg.Header.Set(jetstream.UserID, userID)

	presence, err := natsClient.RequestMsg(msg, time.Second*10) //nolint:mnd
	if err != nil {
		log.WithError(err).Errorf("unable to get presence")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	statusMsg := presence.Header.Get("status_msg")
	e := presence.Header.Get("error")
	if e != "" {
		log.Errorf("received error msg from nats: %s", e)
		return util.JSONResponse{
			Code: http.StatusOK,
			JSON: types.PresenceClientResponse{
				Presence: types.PresenceUnavailable.String(),
			},
		}
	}
	lastActive, err := strconv.Atoi(presence.Header.Get("last_active_ts"))
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}

	p := types.PresenceInternal{LastActiveTS: spec.Timestamp(lastActive)}
	currentlyActive := p.CurrentlyActive()
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: types.PresenceClientResponse{
			CurrentlyActive: &currentlyActive,
			LastActiveAgo:   p.LastActiveAgo(),
			Presence:        presence.Header.Get("presence"),
			StatusMsg:       &statusMsg,
		},
	}
}
