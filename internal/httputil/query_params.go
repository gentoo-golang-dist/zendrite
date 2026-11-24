// Copyright 2026 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package httputil

import (
	"net/http"
	"sort"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

// repeatableQueryParams are the query parameters the Matrix APIs define as
// arrays, which a request may give more than once.
var repeatableQueryParams = map[string]struct{}{
	// Client-server: join, knock, room summary and hierarchy.
	"via":                    {},
	"org.matrix.msc4235.via": {},
	"server_name":            {},
	// Federation: make_join, make_knock and peek take ver, backfill takes v.
	"ver": {},
	"v":   {},
}

// WithAllowRepeatedQueryParams lets an endpoint take any query parameter more
// than once, for endpoints that pass the query on as is, such as the
// third-party lookups.
func WithAllowRepeatedQueryParams() AuthAPIOption {
	return func(opts *AuthAPIOpts) {
		opts.RepeatedQueryParamsAllowed = true
	}
}

// RejectRepeatedQueryParams returns M_INVALID_PARAM when the request gives a
// single-valued query parameter more than once, such as ?dir=b&dir=f, which a
// handler reading the first value would otherwise accept. It returns nil when
// every parameter appears at most once or is an array parameter.
func RejectRepeatedQueryParams(req *http.Request) *util.JSONResponse {
	query := req.URL.Query()
	var repeated []string
	for name, values := range query {
		if len(values) < 2 { //nolint:mnd
			continue
		}
		if _, ok := repeatableQueryParams[name]; !ok {
			repeated = append(repeated, name)
		}
	}
	if len(repeated) == 0 {
		return nil
	}
	sort.Strings(repeated)
	return &util.JSONResponse{
		Code: http.StatusBadRequest,
		JSON: spec.InvalidParam("query parameter " + repeated[0] + " must be given at most once"),
	}
}

// buildAuthAPIOpts applies the options to a fresh AuthAPIOpts.
func buildAuthAPIOpts(checks []AuthAPIOption) AuthAPIOpts {
	opts := AuthAPIOpts{}
	for _, opt := range checks {
		opt(&opts)
	}
	return opts
}
