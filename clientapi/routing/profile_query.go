// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	appserviceAPI "codefloe.com/pat-s/zendrite/appservice/api"
	"codefloe.com/pat-s/zendrite/clientapi/httputil"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// Limits of a profile query (MSC4536). The body limit leaves room for 100
// long user IDs and 30 field names.
const (
	profileQueryMaxUsers     = 100
	profileQueryMaxFields    = 30
	profileQueryMaxBodyBytes = 64 * 1024
)

// QueryProfiles implements POST /profile/query (MSC4536), which returns the
// profiles of up to 100 users, optionally only some of their fields. Only
// local profiles are returned: the MSC asks servers not to look profiles up
// over federation for it, and to answer from what they hold, so users that
// an application service has not registered yet are not looked up either.
// Users without a profile, or without any of the requested fields, are left
// out, and so are those whose profile cannot be read.
func QueryProfiles(req *http.Request, profileAPI userapi.ProfileAPI, cfg *config.ClientAPI) util.JSONResponse {
	body, err := io.ReadAll(io.LimitReader(req.Body, profileQueryMaxBodyBytes+1))
	if err != nil {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("The request body could not be read.")}
	}
	if len(body) > profileQueryMaxBodyBytes {
		return util.JSONResponse{Code: http.StatusRequestEntityTooLarge, JSON: spec.TooLarge("The request body is too large.")}
	}
	users, fields, errRes := parseProfileQuery(body)
	if errRes != nil {
		return *errRes
	}

	profiles := make(map[string]map[string]any, len(users))
	for _, userID := range users {
		if !cfg.Matrix.IsLocalServerName(userID.Domain()) {
			continue
		}
		profile, err := profileAPI.QueryProfile(req.Context(), userID.String())
		switch {
		case errors.Is(err, appserviceAPI.ErrProfileNotExists):
			continue
		case err != nil:
			util.GetLogger(req.Context()).WithError(err).WithField("user_id", userID.String()).Warn("Leaving a profile out of a profile query")
			continue
		}
		all := profile.ClientFields()
		if fields != nil {
			selected := make(map[string]any, len(fields))
			for _, field := range fields {
				if value, ok := all[field]; ok {
					selected[field] = value
				}
			}
			all = selected
		}
		if len(all) > 0 {
			profiles[userID.String()] = all
		}
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: profiles}
}

// parseProfileQuery validates a profile query and returns its users, without
// duplicates, and its fields, which are nil when the query asks for every
// field.
func parseProfileQuery(body []byte) ([]spec.UserID, []string, *util.JSONResponse) {
	badJSON := func(format string, args ...any) *util.JSONResponse {
		return &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON(fmt.Sprintf(format, args...))}
	}
	var request map[string]json.RawMessage
	if errRes := httputil.UnmarshalJSON(body, &request); errRes != nil {
		return nil, nil, errRes
	}
	if request == nil {
		return nil, nil, badJSON("The request body must be a JSON object.")
	}
	rawUsers, ok := request["users"]
	if !ok {
		return nil, nil, &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("users is required.")}
	}
	var userIDs []string
	if err := json.Unmarshal(rawUsers, &userIDs); err != nil || userIDs == nil {
		return nil, nil, badJSON("users must be a list of user IDs.")
	}
	if len(userIDs) == 0 || len(userIDs) > profileQueryMaxUsers {
		return nil, nil, badJSON("users must list between 1 and %d user IDs.", profileQueryMaxUsers)
	}
	users := make([]spec.UserID, 0, len(userIDs))
	seen := make(map[string]struct{}, len(userIDs))
	for _, raw := range userIDs {
		userID, err := spec.NewUserID(raw, false)
		if err != nil {
			return nil, nil, badJSON("users contains an invalid user ID: %q.", raw)
		}
		if _, ok := seen[userID.String()]; ok {
			continue
		}
		seen[userID.String()] = struct{}{}
		users = append(users, *userID)
	}

	rawFields, ok := request["fields"]
	if !ok {
		return users, nil, nil
	}
	var fields []string
	if err := json.Unmarshal(rawFields, &fields); err != nil || fields == nil {
		return nil, nil, badJSON("fields must be a list of field names.")
	}
	if len(fields) == 0 || len(fields) > profileQueryMaxFields {
		return nil, nil, badJSON("fields must list between 1 and %d field names.", profileQueryMaxFields)
	}
	for _, field := range fields {
		if field == "" || len(field) > maxProfileKeyLength {
			return nil, nil, badJSON("fields must be names of 1 to %d bytes.", maxProfileKeyLength)
		}
	}
	return users, fields, nil
}
