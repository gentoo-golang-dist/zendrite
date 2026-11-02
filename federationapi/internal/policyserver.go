// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/federationapi/api"
)

// Paths of the policy server /sign endpoint (MSC4284). The unstable path is
// tried when a policy server does not implement the stable one yet.
const (
	policyServerSignPath         = "/_matrix/policy/v1/sign"
	unstablePolicyServerSignPath = "/_matrix/policy/unstable/org.matrix.msc4284/sign"
)

// policyServerTimeout bounds a /sign request. The room's input waits for the
// answer, so a slow policy server must not hold it up for long.
const policyServerTimeout = 10 * time.Second

// PolicyServerSign implements api.RoomserverFederationAPI.
func (r *FederationInternalAPI) PolicyServerSign(
	ctx context.Context, origin, policyServer spec.ServerName, event gomatrixserverlib.PDU,
) (map[string]map[string]spec.Base64Bytes, error) {
	identity, err := r.cfg.Matrix.SigningIdentityFor(origin)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, policyServerTimeout)
	defer cancel()

	for _, path := range []string{policyServerSignPath, unstablePolicyServerSignPath} {
		req := fclient.NewFederationRequest(http.MethodPost, origin, policyServer, path)
		if err = req.SetContent(json.RawMessage(event.JSON())); err != nil {
			return nil, err
		}
		if err = req.Sign(identity.ServerName, identity.KeyID, identity.PrivateKey); err != nil {
			return nil, err
		}
		httpReq, reqErr := req.HTTPRequest()
		if reqErr != nil {
			return nil, reqErr
		}
		var signatures map[string]map[string]spec.Base64Bytes
		err = r.federation.DoRequestAndParseResponse(ctx, httpReq, &signatures)
		if err == nil {
			return signatures, nil
		}
		var httpErr spec.HTTPError
		if !errors.As(err, &httpErr) {
			return nil, fmt.Errorf("policy server %s: %w", policyServer, err)
		}
		var respErr spec.RespError
		_ = json.Unmarshal(httpErr.Contents, &respErr)
		unrecognized := (httpErr.Code == http.StatusNotFound || httpErr.Code == http.StatusMethodNotAllowed) &&
			(respErr.ErrCode == "" || respErr.ErrCode == string(spec.ErrorUnrecognized))
		switch {
		case unrecognized && path == policyServerSignPath:
			// Try the unstable endpoint.
			continue
		case unrecognized, httpErr.Code == http.StatusTooManyRequests,
			httpErr.Code < http.StatusBadRequest, httpErr.Code >= http.StatusInternalServerError:
			// The policy server cannot answer: it is not one, is rate
			// limiting us or has failed.
			return nil, fmt.Errorf("policy server %s: %w", policyServer, err)
		default:
			errCode := respErr.ErrCode
			if errCode == "" {
				errCode = string(spec.ErrorForbidden)
			}
			return nil, &api.PolicyServerRefusal{StatusCode: httpErr.Code, ErrCode: errCode, Message: respErr.Err}
		}
	}
	return nil, fmt.Errorf("policy server %s: %w", policyServer, err)
}
