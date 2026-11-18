// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/clientapi/httputil"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

type uploadKeysRequest struct {
	DeviceKeys   json.RawMessage            `json:"device_keys"`
	OneTimeKeys  map[string]json.RawMessage `json:"one_time_keys"`
	FallbackKeys map[string]json.RawMessage `json:"fallback_keys"`
}

func UploadKeys(req *http.Request, keyAPI api.ClientKeyAPI, device *api.Device) util.JSONResponse {
	var r uploadKeysRequest
	resErr := httputil.UnmarshalJSONRequest(req, &r)
	if resErr != nil {
		return *resErr
	}

	if err := validateDeviceKeys(r.DeviceKeys); err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("Invalid device_keys: " + err.Error()),
		}
	}
	if err := validateKeyIDs(r.OneTimeKeys); err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("Invalid one_time_keys: " + err.Error()),
		}
	}
	if err := validateKeyIDs(r.FallbackKeys); err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("Invalid fallback_keys: " + err.Error()),
		}
	}

	uploadReq := &api.PerformUploadKeysRequest{
		DeviceID: device.ID,
		UserID:   device.UserID,
	}
	if r.DeviceKeys != nil {
		uploadReq.DeviceKeys = []api.DeviceKeys{
			{
				DeviceID: device.ID,
				UserID:   device.UserID,
				KeyJSON:  r.DeviceKeys,
			},
		}
	}
	if r.OneTimeKeys != nil {
		uploadReq.OneTimeKeys = []api.OneTimeKeys{
			{
				DeviceID: device.ID,
				UserID:   device.UserID,
				KeyJSON:  r.OneTimeKeys,
			},
		}
	}

	if r.FallbackKeys != nil {
		uploadReq.FallbackKeys = []api.FallbackKeys{{UserID: device.UserID, DeviceID: device.ID, KeyJSON: r.FallbackKeys}}
	}

	var uploadRes api.PerformUploadKeysResponse
	if err := keyAPI.PerformUploadKeys(req.Context(), uploadReq, &uploadRes); err != nil {
		return util.ErrorResponse(err)
	}
	if uploadRes.Error != nil {
		util.GetLogger(req.Context()).WithError(uploadRes.Error).Error("Failed to PerformUploadKeys")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	if len(uploadRes.KeyErrors) > 0 {
		util.GetLogger(req.Context()).WithField("key_errors", uploadRes.KeyErrors).Error("Failed to upload one or more keys")
		return util.JSONResponse{
			Code: 400, //nolint:mnd
			JSON: uploadRes.KeyErrors,
		}
	}
	keyCount := make(map[string]int)
	if len(uploadRes.OneTimeKeyCounts) > 0 {
		keyCount = uploadRes.OneTimeKeyCounts[0].KeyCount
	}
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: struct {
			OTKCounts any `json:"one_time_key_counts"`
		}{keyCount},
	}
}

// validateDeviceKeys checks that device_keys, when given, has the fields
// the spec requires: user_id, device_id, algorithms, keys and signatures.
func validateDeviceKeys(deviceKeys json.RawMessage) error {
	if len(deviceKeys) == 0 || string(deviceKeys) == "null" {
		return nil
	}
	var keys struct {
		UserID     *string                      `json:"user_id"`
		DeviceID   *string                      `json:"device_id"`
		Algorithms *[]string                    `json:"algorithms"`
		Keys       map[string]string            `json:"keys"`
		Signatures map[string]map[string]string `json:"signatures"`
	}
	if err := json.Unmarshal(deviceKeys, &keys); err != nil {
		return err
	}
	switch {
	case keys.UserID == nil:
		return errors.New("missing user_id")
	case keys.DeviceID == nil:
		return errors.New("missing device_id")
	case keys.Algorithms == nil:
		return errors.New("missing algorithms")
	case keys.Keys == nil:
		return errors.New("missing keys")
	case keys.Signatures == nil:
		return errors.New("missing signatures")
	}
	return nil
}

// validateKeyIDs checks that every key in a one_time_keys or fallback_keys
// map is named <algorithm>:<key_id>, with neither part empty.
func validateKeyIDs(keys map[string]json.RawMessage) error {
	for keyIDWithAlgo := range keys {
		algo, keyID, ok := strings.Cut(keyIDWithAlgo, ":")
		if !ok || algo == "" || keyID == "" {
			return fmt.Errorf("key %q is not of the form <algorithm>:<key_id>", keyIDWithAlgo)
		}
	}
	return nil
}

type queryKeysRequest struct {
	Timeout    int                 `json:"timeout"`
	DeviceKeys map[string][]string `json:"device_keys"`
}

func (r *queryKeysRequest) GetTimeout() time.Duration {
	if r.Timeout == 0 {
		return 10 * time.Second //nolint:mnd
	}
	timeout := time.Duration(r.Timeout) * time.Millisecond
	if timeout > time.Second*20 {
		timeout = time.Second * 20 //nolint:mnd
	}
	return timeout
}

// usersSharingRoom returns those of otherUserIDs that share a room with
// userID, as RoomserverInternalAPI.QueryUsersSharingRoom does.
type usersSharingRoom func(ctx context.Context, userID string, otherUserIDs []string) ([]string, error)

// QueryKeys implements POST /keys/query. With sharingRoom set, it leaves out
// the users who do not share a room with the requester (MSC4263).
func QueryKeys(req *http.Request, keyAPI api.ClientKeyAPI, device *api.Device, sharingRoom usersSharingRoom) util.JSONResponse {
	var r queryKeysRequest
	resErr := httputil.UnmarshalJSONRequest(req, &r)
	if resErr != nil {
		return *resErr
	}
	if sharingRoom != nil {
		if err := limitKeyQuery(req.Context(), r.DeviceKeys, device.UserID, sharingRoom); err != nil {
			util.GetLogger(req.Context()).WithError(err).Error("failed to find the users sharing a room with the requester")
			return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
		}
	}
	queryRes := api.QueryKeysResponse{}
	keyAPI.QueryKeys(req.Context(), &api.QueryKeysRequest{
		UserID:        device.UserID,
		UserToDevices: r.DeviceKeys,
		Timeout:       r.GetTimeout(),
	}, &queryRes)
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: map[string]any{
			"device_keys":       queryRes.DeviceKeys,
			"master_keys":       queryRes.MasterKeys,
			"self_signing_keys": queryRes.SelfSigningKeys,
			"user_signing_keys": queryRes.UserSigningKeys,
			"failures":          queryRes.Failures,
		},
	}
}

// limitKeyQuery removes the users who do not share a room with userID from
// a key query, except userID itself.
func limitKeyQuery(ctx context.Context, query map[string][]string, userID string, sharingRoom usersSharingRoom) error {
	others := make([]string, 0, len(query))
	for other := range query {
		if other != userID {
			others = append(others, other)
		}
	}
	if len(others) == 0 {
		return nil
	}
	sharing, err := sharingRoom(ctx, userID, others)
	if err != nil {
		return err
	}
	allowed := make(map[string]struct{}, len(sharing))
	for _, other := range sharing {
		allowed[other] = struct{}{}
	}
	for _, other := range others {
		if _, ok := allowed[other]; !ok {
			delete(query, other)
		}
	}
	return nil
}

type claimKeysRequest struct {
	TimeoutMS int `json:"timeout"`
	//  The keys to be claimed. A map from user ID, to a map from device ID to algorithm name.
	OneTimeKeys map[string]map[string]string `json:"one_time_keys"`
}

func (r *claimKeysRequest) GetTimeout() time.Duration {
	if r.TimeoutMS == 0 {
		return 10 * time.Second //nolint:mnd
	}
	return time.Duration(r.TimeoutMS) * time.Millisecond
}

func ClaimKeys(req *http.Request, keyAPI api.ClientKeyAPI) util.JSONResponse {
	var r claimKeysRequest
	resErr := httputil.UnmarshalJSONRequest(req, &r)
	if resErr != nil {
		return *resErr
	}
	claimRes := api.PerformClaimKeysResponse{}
	keyAPI.PerformClaimKeys(req.Context(), &api.PerformClaimKeysRequest{
		OneTimeKeys: r.OneTimeKeys,
		Timeout:     r.GetTimeout(),
	}, &claimRes)
	if claimRes.Error != nil {
		util.GetLogger(req.Context()).WithError(claimRes.Error).Error("failed to PerformClaimKeys")
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: spec.InternalServerError{},
		}
	}
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: map[string]any{
			"one_time_keys": claimRes.OneTimeKeys,
			"failures":      claimRes.Failures,
		},
	}
}
