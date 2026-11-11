// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"codefloe.com/pat-s/zendrite/federationapi/api"
	"codefloe.com/pat-s/zendrite/federationapi/queue"
	"codefloe.com/pat-s/zendrite/federationapi/statistics"
	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/process"
	"codefloe.com/pat-s/zendrite/test"
)

// versionedJoinClient answers make_join with a template in a fixed room
// version and records whether send_join was attempted.
type versionedJoinClient struct {
	fclient.FederationClient
	roomVersion gomatrixserverlib.RoomVersion
	sentJoin    *bool
}

func (c versionedJoinClient) MakeJoin(_ context.Context, _, _ spec.ServerName, roomID, userID string) (fclient.RespMakeJoin, error) {
	return fclient.RespMakeJoin{
		RoomVersion: c.roomVersion,
		JoinEvent: gomatrixserverlib.ProtoEvent{
			Type:     spec.MRoomMember,
			RoomID:   roomID,
			SenderID: userID,
			StateKey: &userID,
		},
	}, nil
}

func (c versionedJoinClient) SendJoin(context.Context, spec.ServerName, spec.ServerName, gomatrixserverlib.PDU) (fclient.RespSendJoin, error) {
	*c.sentJoin = true
	return fclient.RespSendJoin{}, spec.HTTPError{Code: http.StatusForbidden, Contents: []byte(`{"errcode":"M_FORBIDDEN"}`)}
}

func newVersionedJoinAPI(t *testing.T, roomVersion gomatrixserverlib.RoomVersion) (*FederationInternalAPI, *statistics.Statistics, *bool) {
	t.Helper()
	testDB := test.NewInMemoryFederationDatabase()
	_, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	cfg := config.FederationAPI{Matrix: &config.Global{
		SigningIdentity: fclient.SigningIdentity{ServerName: "local", KeyID: "ed25519:1", PrivateKey: key},
	}}
	sentJoin := new(bool)
	fedClient := versionedJoinClient{roomVersion: roomVersion, sentJoin: sentJoin}
	stats := statistics.NewStatistics(testDB, FailuresUntilBlacklist, FailuresUntilAssumedOffline, false)
	queues := queue.NewOutgoingQueues(testDB, process.NewProcessContext(), false, cfg.Matrix.ServerName, fedClient, &stats, nil)
	return NewFederationInternalAPI(testDB, &cfg, nil, fedClient, &stats, nil, queues, nil), &stats, sentJoin
}

// gomatrixserverlib offers every room version it knows in the make_join ver
// list. A template for a version this server does not implement (MSC4242
// state DAGs) is refused with 400 M_INCOMPATIBLE_ROOM_VERSION before
// send_join, without backing off the remote server.
func TestJoinRefusesUnimplementedRoomVersion(t *testing.T) {
	fedAPI, stats, sentJoin := newVersionedJoinAPI(t, gomatrixserverlib.RoomVersionStateDAGs)
	res := api.PerformJoinResponse{}
	fedAPI.PerformJoin(context.Background(), &api.PerformJoinRequest{
		RoomID:      "!AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		UserID:      "@alice:local",
		ServerNames: []spec.ServerName{"remote"},
	}, &res)
	require.NotNil(t, res.LastError)
	assert.Equal(t, http.StatusBadRequest, res.LastError.Code, res.LastError.Message)
	assert.Equal(t, "M_INCOMPATIBLE_ROOM_VERSION", gjson.Get(res.LastError.Message, "errcode").Str, res.LastError.Message)
	assert.Equal(t, string(gomatrixserverlib.RoomVersionStateDAGs), gjson.Get(res.LastError.Message, "room_version").Str)
	assert.False(t, *sentJoin, "no send_join for an unimplemented room version")
	assert.Zero(t, stats.ForServer("remote").FailureCount(), "an unimplemented room version must not back off the server")

	// A template in an implemented version passes the check.
	fedAPI, _, _ = newVersionedJoinAPI(t, gomatrixserverlib.RoomVersionV12)
	makeJoin, err := fedAPI.MakeJoin(context.Background(), "local", "remote", "!AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "@alice:local")
	require.NoError(t, err)
	assert.Equal(t, gomatrixserverlib.RoomVersionV12, makeJoin.GetRoomVersion())
}
