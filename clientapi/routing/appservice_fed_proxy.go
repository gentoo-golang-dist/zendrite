// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/clientapi/httputil"
	"codefloe.com/pat-s/zendrite/internal/appserviceproxy"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// The unstable error codes of the federation proxy (MSC4512), as Synapse
// spells them.
const (
	errCodeFedProxyDestinationDenied = "IO.ELEMENT.MSC4512.M_FEDPROXY_DESTINATION_DENIED"
	errCodeFedProxyPathNotAllowed    = "IO.ELEMENT.MSC4512.M_FEDPROXY_PATH_NOT_ALLOWED"
	errCodeFedProxyConnectionFailed  = "IO.ELEMENT.MSC4512.M_FEDPROXY_CONNECTION_FAILED"
)

// fedProxyTimeout bounds a federation request sent for an application
// service.
const fedProxyTimeout = 30 * time.Second

// fedProxyResponse relays the destination's answer to the application
// service.
type fedProxyResponse struct {
	Status  int             `json:"status"`
	Content json.RawMessage `json:"content,omitempty"`
}

// AppserviceFederationProxy implements
// POST /_matrix/client/unstable/io.element.msc4512/appservice/fed_proxy
// (MSC4512): it signs and sends a federation request under the application
// service's proxy prefix to another server and relays the answer.
func AppserviceFederationProxy(
	req *http.Request, device *userapi.Device, cfg *config.ClientAPI,
	appservices []config.ApplicationService, federation fclient.FederationClient,
) util.JSONResponse {
	var as *config.ApplicationService
	for i := range appservices {
		if device.AppserviceID != "" && appservices[i].ID == device.AppserviceID {
			as = &appservices[i]
			break
		}
	}
	if as == nil {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("Only application services can use this endpoint")}
	}

	var fields map[string]json.RawMessage
	if resErr := httputil.UnmarshalJSONRequest(req, &fields); resErr != nil {
		return *resErr
	}
	destination, resErr := fedProxyString(fields, "destination")
	if resErr != nil {
		return *resErr
	}
	method, resErr := fedProxyString(fields, "method")
	if resErr != nil {
		return *resErr
	}
	path, resErr := fedProxyString(fields, "path")
	if resErr != nil {
		return *resErr
	}
	switch method {
	case http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete:
	default:
		return fedProxyInvalid("method must be one of GET, PUT, POST and DELETE")
	}
	var query map[string]string
	if raw, ok := fields["query"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &query); err != nil {
			return fedProxyInvalid("query must be an object of strings")
		}
	}
	var body json.RawMessage
	if raw, ok := fields["body"]; ok && string(raw) != "null" {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return fedProxyInvalid("body must be an object")
		}
		if method == http.MethodGet || method == http.MethodDelete {
			return fedProxyInvalid("body is not allowed for " + method)
		}
		body = raw
	}
	host, _, ok := spec.ParseAndValidateServerName(spec.ServerName(destination))
	if !ok {
		return fedProxyInvalid("destination is not a valid server name")
	}

	// The server name with an explicit port still names this server.
	if cfg.Matrix.DisableFederation || cfg.Matrix.IsLocalServerName(spec.ServerName(destination)) ||
		cfg.Matrix.IsLocalServerName(spec.ServerName(host)) {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.MatrixError{
			ErrCode: errCodeFedProxyDestinationDenied, Err: "Federation requests to this destination are not allowed",
		}}
	}
	if !fedProxyPathAllowed(as, path) {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.MatrixError{
			ErrCode: errCodeFedProxyPathNotAllowed,
			Err:     "path must be under /_matrix/federation/<version>/ and the proxy prefix of the application service",
		}}
	}

	requestURI := path
	if len(query) > 0 {
		values := url.Values{}
		for key, value := range query {
			values.Set(key, value)
		}
		requestURI += "?" + values.Encode()
	}
	fedReq := fclient.NewFederationRequest(method, cfg.Matrix.ServerName, spec.ServerName(destination), requestURI)
	if body != nil {
		if err := fedReq.SetContent(body); err != nil {
			return fedProxyInvalid("body could not be encoded")
		}
	}
	if err := fedReq.Sign(cfg.Matrix.ServerName, cfg.Matrix.KeyID, cfg.Matrix.PrivateKey); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("Signing the proxied federation request failed")
		return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
	}
	httpReq, err := fedReq.HTTPRequest()
	if err != nil {
		return fedProxyInvalid("path could not be encoded")
	}

	ctx, cancel := context.WithTimeout(req.Context(), fedProxyTimeout)
	defer cancel()
	var content json.RawMessage
	err = federation.DoRequestAndParseResponse(ctx, httpReq, &content)
	var httpErr spec.HTTPError
	var urlErr *url.Error
	switch {
	// A 2xx answer without a body fails to decode with io.EOF, which a
	// dropped connection also reports, wrapped in a url.Error.
	case err == nil, errors.Is(err, io.EOF) && !errors.As(err, &urlErr):
		return util.JSONResponse{Code: http.StatusOK, JSON: fedProxyResponse{Status: http.StatusOK, Content: content}}
	case errors.As(err, &httpErr):
		res := fedProxyResponse{Status: httpErr.Code}
		if json.Valid(httpErr.Contents) {
			res.Content = httpErr.Contents
		}
		return util.JSONResponse{Code: http.StatusOK, JSON: res}
	default:
		util.GetLogger(req.Context()).WithError(err).WithField("appservice_id", as.ID).
			WithField("destination", destination).Warn("Sending a federation request for an application service failed")
		return util.JSONResponse{Code: http.StatusBadGateway, JSON: spec.MatrixError{
			ErrCode: errCodeFedProxyConnectionFailed, Err: "The request could not be delivered to the destination",
		}}
	}
}

// fedProxyString returns a required string field of a fed_proxy request.
func fedProxyString(fields map[string]json.RawMessage, name string) (string, *util.JSONResponse) {
	raw, ok := fields[name]
	if !ok || string(raw) == "null" {
		return "", &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam(name + " is required")}
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		res := fedProxyInvalid(name + " must be a string")
		return "", &res
	}
	if value == "" {
		return "", &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam(name + " is required")}
	}
	return value, nil
}

func fedProxyInvalid(msg string) util.JSONResponse {
	return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam(msg)}
}

// fedProxyPathAllowed reports whether the application service may send a
// federation request to path: one under its own proxy prefix, without a query
// string, fragment or dot segment.
func fedProxyPathAllowed(as *config.ApplicationService, path string) bool {
	if as.ProxyRules() == nil || strings.ContainsAny(path, "?#") || appserviceproxy.HasDotSegments(path) {
		return false
	}
	return appserviceproxy.PathPattern(appserviceproxy.FederationAPI, as.ProxyPrefix).MatchString(path)
}
