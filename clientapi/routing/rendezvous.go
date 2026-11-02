// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/clientapi/auth"
	"codefloe.com/pat-s/zendrite/internal/httputil"
	"codefloe.com/pat-s/zendrite/setup/config"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// QR code login (MSC4108) runs over a rendezvous session: a small payload
// on the homeserver that two devices, one of them not signed in, take turns
// to read and replace. Zendrite serves two APIs for it:
//
//   - MSC4388, which the current MSC4108 text builds on, under
//     /_matrix/client/unstable/io.element.msc4388/rendezvous: JSON bodies and
//     a compare-and-swap on sequence_token, with idempotent transaction IDs.
//   - The 2024 MSC4108 API under
//     /_matrix/client/unstable/org.matrix.msc4108/rendezvous: text/plain
//     bodies and a compare-and-swap on the ETag. Element X and
//     matrix-rust-sdk still create their sessions through it.
//
// Sessions live in the user API database, so every HA replica sees them.
const (
	// Payloads are at most 4096 bytes, as MSC4388 requires.
	rendezvousMaxPayload = 4096
	// The JSON request body may be larger than the payload it carries,
	// because of escaping.
	rendezvousMaxBody = 8 * rendezvousMaxPayload
	// A session remembers this many transaction IDs. An exchange needs a
	// handful.
	rendezvousMaxTxns = 100
	// The session ID is the only secret that guards a session.
	rendezvousIDBytes = 24
	// A user may have this many active sessions. A QR code login needs one,
	// and a client may start over a few times.
	rendezvousMaxPerUser = 10
)

const (
	msc4388ConcurrentWrite spec.MatrixErrorCode = "IO_ELEMENT_MSC4388_CONCURRENT_WRITE"
	msc4108SessionPath     string               = "/_matrix/client/unstable/org.matrix.msc4108/rendezvous/"
)

// opaqueIDPattern is the opaque identifier grammar of the spec.
var opaqueIDPattern = regexp.MustCompile(`^[0-9A-Za-z._~-]{1,255}$`)

type rendezvous struct {
	mscCfg       *config.MSCs
	clientCfg    *config.ClientAPI
	userAPI      userapi.ClientUserAPI
	limits       *httputil.RateLimits
	createLimits *httputil.RateLimits
	now          func() time.Time
}

func newRendezvous(mscCfg *config.MSCs, clientCfg *config.ClientAPI, userAPI userapi.ClientUserAPI, limits, createLimits *httputil.RateLimits) *rendezvous {
	return &rendezvous{
		mscCfg:       mscCfg,
		clientCfg:    clientCfg,
		userAPI:      userAPI,
		limits:       limits,
		createLimits: createLimits,
		now:          time.Now,
	}
}

// register adds the enabled rendezvous APIs to the unstable client router.
func (r *rendezvous) register(unstableMux *httputil.Router) {
	if r.mscCfg.MSC4388Mode() != config.RendezvousModeOff {
		unstableMux.Handle("/io.element.msc4388/rendezvous",
			httputil.MakeExternalAPI("rendezvous_discover", r.discover),
		).Methods(http.MethodGet, http.MethodOptions)
		unstableMux.Handle("/io.element.msc4388/rendezvous",
			httputil.MakeExternalAPI("rendezvous_create", r.create),
		).Methods(http.MethodPost)
		unstableMux.Handle("/io.element.msc4388/rendezvous/{sessionID}",
			httputil.MakeExternalAPI("rendezvous_get", r.get),
		).Methods(http.MethodGet, http.MethodOptions)
		unstableMux.Handle("/io.element.msc4388/rendezvous/{sessionID}",
			httputil.MakeExternalAPI("rendezvous_delete", r.delete),
		).Methods(http.MethodDelete)
		unstableMux.Handle("/io.element.msc4388/rendezvous/{sessionID}/{txnID}",
			httputil.MakeExternalAPI("rendezvous_send", r.send),
		).Methods(http.MethodPut, http.MethodOptions)
	}
	if r.mscCfg.MSC4108Enabled() {
		unstableMux.Handle("/org.matrix.msc4108/rendezvous",
			r.msc4108Handler(r.msc4108Create),
		).Methods(http.MethodPost, http.MethodOptions)
		unstableMux.Handle("/org.matrix.msc4108/rendezvous/{sessionID}",
			r.msc4108Handler(r.msc4108Session),
		).Methods(http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodOptions)
	}
}

func (r *rendezvous) timestamp() spec.Timestamp {
	return spec.AsTimestamp(r.now())
}

func (r *rendezvous) expiresInMS(expires spec.Timestamp) int64 {
	ms := int64(expires) - int64(r.timestamp())
	if ms < 0 {
		return 0
	}
	return ms
}

// limit applies the general rendezvous rate limit, per address. It runs
// before authentication, so that bad tokens cannot flood the provider's
// introspection endpoint.
func (r *rendezvous) limit(req *http.Request) *util.JSONResponse {
	return r.limits.Limit(req, nil)
}

// limitCreate applies the creation rate limit, per device when there is one
// and per address otherwise.
func (r *rendezvous) limitCreate(req *http.Request, device *userapi.Device) *util.JSONResponse {
	return r.createLimits.Limit(req, device)
}

// optionalDevice authenticates a request that may carry an access token. A
// request without one has no device; one with a bad token fails as on any
// authenticated endpoint, as MSC4388 requires, so that a client can tell a
// token that needs refreshing from a server that refuses it.
func (r *rendezvous) optionalDevice(req *http.Request) (*userapi.Device, *util.JSONResponse) {
	if req.Header.Get("Authorization") == "" && req.URL.Query().Get("access_token") == "" {
		return nil, nil
	}
	device, errRes := auth.VerifyUserFromRequest(req, r.userAPI)
	if errRes != nil {
		return nil, errRes
	}
	if device.Locked {
		res := auth.UserLockedResponse()
		return nil, &res
	}
	return device, nil
}

// mayCreate applies the MSC4388 creation policy.
func (r *rendezvous) mayCreate(device *userapi.Device) bool {
	if r.mscCfg.MSC4388Mode() == config.RendezvousModeOpen {
		return true
	}
	if device == nil || device.Suspended {
		return false
	}
	switch device.AccountType {
	case userapi.AccountTypeGuest, userapi.AccountTypeCallGuest:
		return false
	}
	return true
}

// newSession stores a new session with the given payload.
func (r *rendezvous) newSession(req *http.Request, kind userapi.RendezvousKind, data []byte, contentType string, device *userapi.Device) (*userapi.RendezvousSession, *util.JSONResponse) {
	raw := make([]byte, rendezvousIDBytes)
	if _, err := rand.Read(raw); err != nil {
		res := util.ErrorResponse(err)
		return nil, &res
	}
	// Guests and suspended accounts may only create sessions in open mode,
	// and count as anonymous there.
	anonymous := device == nil || device.Suspended ||
		device.AccountType == userapi.AccountTypeGuest || device.AccountType == userapi.AccountTypeCallGuest
	var userID string
	if !anonymous {
		userID = device.UserID
	}
	now := r.timestamp()
	session := &userapi.RendezvousSession{
		ID:           base64.RawURLEncoding.EncodeToString(raw),
		Kind:         kind,
		Data:         data,
		ContentType:  contentType,
		Sequence:     1,
		LastModified: now,
		Expires:      now + spec.Timestamp(r.mscCfg.Rendezvous.SessionLifetime.Milliseconds()),
		Anonymous:    anonymous,
		UserID:       userID,
	}
	err := r.userAPI.PerformRendezvousCreate(req.Context(), session, r.mscCfg.Rendezvous.MaxSessions, rendezvousMaxPerUser)
	if errors.Is(err, userapi.ErrRendezvousLimit) {
		res := httputil.LimitExceededResponse("Too many rendezvous sessions are active", r.mscCfg.Rendezvous.SessionLifetime)
		return nil, &res
	}
	if err != nil {
		res := rendezvousInternalError(req, err, "PerformRendezvousCreate")
		return nil, &res
	}
	return session, nil
}

// unsafeFetch reports why a browser request for a session payload may end up
// rendered to a user: MSC4388 asks servers to refuse top-level navigation,
// so that a session cannot serve content to someone who opens its link.
func unsafeFetch(req *http.Request) string {
	if dest := req.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "empty" {
		return "Rendezvous content is not accessible from the request destination"
	}
	if req.Header.Get("Sec-Fetch-Mode") == "navigate" {
		return "Rendezvous content is not accessible via top-level navigation"
	}
	if req.Header.Get("Sec-Fetch-User") == "?1" {
		return "Rendezvous content is not accessible from requests with user activation"
	}
	if req.Header.Get("Sec-Fetch-Site") == "none" {
		return "Rendezvous content is not accessible from requests from user interaction"
	}
	return ""
}

// rendezvousVars returns the decoded path variables. Undecodable values come
// back empty, which no session or transaction ID matches.
func rendezvousVars(req *http.Request) map[string]string {
	vars, err := httputil.URLDecodeMapValues(httputil.Vars(req))
	if err != nil {
		return map[string]string{}
	}
	return vars
}

func rendezvousNotFound() util.JSONResponse {
	return util.JSONResponse{Code: http.StatusNotFound, JSON: spec.NotFound("Rendezvous session not found")}
}

func rendezvousInternalError(req *http.Request, err error, what string) util.JSONResponse {
	util.GetLogger(req.Context()).WithError(err).Error(what + " failed")
	return util.JSONResponse{Code: http.StatusInternalServerError, JSON: spec.InternalServerError{}}
}

// readRendezvousJSON reads a JSON object of string fields for MSC4388.
func readRendezvousJSON(req *http.Request, fields ...string) (map[string]string, *util.JSONResponse) {
	body, err := io.ReadAll(io.LimitReader(req.Body, rendezvousMaxBody+1))
	if err != nil {
		return nil, &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("Could not read the request body")}
	}
	if len(body) > rendezvousMaxBody {
		return nil, &util.JSONResponse{Code: http.StatusRequestEntityTooLarge, JSON: spec.TooLarge("Payload too large")}
	}
	var object map[string]json.RawMessage
	if err = json.Unmarshal(body, &object); err != nil || object == nil {
		return nil, &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.NotJSON("The request body is not a JSON object")}
	}
	values := make(map[string]string, len(fields))
	for _, field := range fields {
		raw, ok := object[field]
		var value string
		if !ok || json.Unmarshal(raw, &value) != nil {
			return nil, &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON(field + " must be a string")}
		}
		values[field] = value
	}
	if len(values["data"]) > rendezvousMaxPayload {
		return nil, &util.JSONResponse{Code: http.StatusRequestEntityTooLarge, JSON: spec.TooLarge("data is larger than 4096 bytes")}
	}
	return values, nil
}

// discover answers GET /rendezvous: whether this requester may create a
// session.
func (r *rendezvous) discover(req *http.Request) util.JSONResponse {
	if res := r.limit(req); res != nil {
		return *res
	}
	device, errRes := r.optionalDevice(req)
	if errRes != nil {
		return *errRes
	}
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: map[string]bool{"create_available": r.mayCreate(device)},
	}
}

// create answers POST /rendezvous.
func (r *rendezvous) create(req *http.Request) util.JSONResponse {
	if res := r.limit(req); res != nil {
		return *res
	}
	device, errRes := r.optionalDevice(req)
	if errRes != nil {
		return *errRes
	}
	if res := r.limitCreate(req, device); res != nil {
		return *res
	}
	if !r.mayCreate(device) {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden("You are not allowed to create a rendezvous session")}
	}
	values, errRes := readRendezvousJSON(req, "data")
	if errRes != nil {
		return *errRes
	}
	session, errRes := r.newSession(req, userapi.RendezvousKindMSC4388, []byte(values["data"]), "", device)
	if errRes != nil {
		return *errRes
	}
	return util.JSONResponse{
		Code: http.StatusOK,
		JSON: map[string]any{
			"id":             session.ID,
			"sequence_token": strconv.FormatInt(session.Sequence, 10),
			"expires_in_ms":  r.expiresInMS(session.Expires),
		},
	}
}

// get answers GET /rendezvous/{sessionID}.
func (r *rendezvous) get(req *http.Request) util.JSONResponse {
	if res := r.limit(req); res != nil {
		return *res
	}
	if reason := unsafeFetch(req); reason != "" {
		return util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden(reason)}
	}
	session, err := r.userAPI.QueryRendezvousSession(req.Context(), rendezvousVars(req)["sessionID"], userapi.RendezvousKindMSC4388, r.timestamp())
	if errors.Is(err, userapi.ErrRendezvousNotFound) {
		return rendezvousNotFound()
	}
	if err != nil {
		return rendezvousInternalError(req, err, "QueryRendezvousSession")
	}
	return util.JSONResponse{
		Code:    http.StatusOK,
		Headers: map[string]string{"Cache-Control": "no-store"},
		JSON: map[string]any{
			"data":           string(session.Data),
			"sequence_token": strconv.FormatInt(session.Sequence, 10),
			"expires_in_ms":  r.expiresInMS(session.Expires),
		},
	}
}

// send answers PUT /rendezvous/{sessionID}/{txnID}.
func (r *rendezvous) send(req *http.Request) util.JSONResponse {
	if res := r.limit(req); res != nil {
		return *res
	}
	vars := rendezvousVars(req)
	sessionID, txnID := vars["sessionID"], vars["txnID"]
	if !opaqueIDPattern.MatchString(txnID) {
		return util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Invalid transaction ID")}
	}
	sequenceResponse := func(sequence int64) util.JSONResponse {
		return util.JSONResponse{
			Code: http.StatusOK,
			JSON: map[string]string{"sequence_token": strconv.FormatInt(sequence, 10)},
		}
	}

	// A recorded transaction is answered before the body is looked at.
	now := r.timestamp()
	recorded, found, err := r.userAPI.QueryRendezvousTxn(req.Context(), sessionID, userapi.RendezvousKindMSC4388, txnID, now)
	if errors.Is(err, userapi.ErrRendezvousNotFound) {
		return rendezvousNotFound()
	}
	if err != nil {
		return rendezvousInternalError(req, err, "QueryRendezvousTxn")
	}
	if found {
		return sequenceResponse(recorded)
	}

	values, errRes := readRendezvousJSON(req, "sequence_token", "data")
	if errRes != nil {
		return *errRes
	}
	concurrentWrite := util.JSONResponse{
		Code: http.StatusConflict,
		JSON: spec.MatrixError{ErrCode: msc4388ConcurrentWrite, Err: "sequence_token does not match"},
	}
	expected, err := strconv.ParseInt(values["sequence_token"], 10, 64)
	if err != nil {
		return concurrentWrite
	}
	written, err := r.userAPI.PerformRendezvousWrite(req.Context(), &userapi.RendezvousWrite{
		ID:               sessionID,
		Kind:             userapi.RendezvousKindMSC4388,
		ExpectedSequence: expected,
		Data:             []byte(values["data"]),
		TxnID:            txnID,
		MaxTxns:          rendezvousMaxTxns,
		Now:              now,
	})
	switch {
	case errors.Is(err, userapi.ErrRendezvousNotFound):
		return rendezvousNotFound()
	case errors.Is(err, userapi.ErrRendezvousConcurrentWrite):
		return concurrentWrite
	case err != nil:
		return rendezvousInternalError(req, err, "PerformRendezvousWrite")
	}
	return sequenceResponse(written.Sequence)
}

// delete answers DELETE /rendezvous/{sessionID}.
func (r *rendezvous) delete(req *http.Request) util.JSONResponse {
	if res := r.limit(req); res != nil {
		return *res
	}
	deleted, err := r.userAPI.PerformRendezvousDelete(req.Context(), rendezvousVars(req)["sessionID"], userapi.RendezvousKindMSC4388, r.timestamp())
	if err != nil {
		return rendezvousInternalError(req, err, "PerformRendezvousDelete")
	}
	if !deleted {
		return rendezvousNotFound()
	}
	return util.JSONResponse{Code: http.StatusOK, JSON: struct{}{}}
}

// msc4108Handler serves the 2024 MSC4108 API, whose bodies are text/plain
// and whose concurrency control uses ETags, so its CORS answer has to let
// browsers send If-Match and If-None-Match and read the ETag.
func (r *rendezvous) msc4108Handler(f func(http.ResponseWriter, *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept, Authorization, If-Match, If-None-Match")
		h.Set("Access-Control-Expose-Headers", "ETag, Expires, Last-Modified")
		if req.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		f(w, req)
	})
}

func writeRendezvousJSON(w http.ResponseWriter, res util.JSONResponse) {
	for k, v := range res.Headers {
		w.Header().Set(k, fmt.Sprint(v))
	}
	body, err := json.Marshal(res.JSON)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.Code)
	_, _ = w.Write(body)
}

// msc4108ETag is the ETag of a session's sequence.
func msc4108ETag(sequence int64) string {
	return `"` + strconv.FormatInt(sequence, 10) + `"`
}

// msc4108ETagSequences parses an If-Match or If-None-Match header into the
// sequences it names. A wildcard matches any sequence.
func msc4108ETagSequences(header string) (sequences []int64, wildcard bool) {
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" {
			wildcard = true
			continue
		}
		tag = strings.TrimPrefix(tag, "W/")
		if len(tag) < 2 || tag[0] != '"' || tag[len(tag)-1] != '"' {
			continue
		}
		if sequence, err := strconv.ParseInt(tag[1:len(tag)-1], 10, 64); err == nil {
			sequences = append(sequences, sequence)
		}
	}
	return sequences, wildcard
}

func setMSC4108SessionHeaders(w http.ResponseWriter, session *userapi.RendezvousSession) {
	h := w.Header()
	h.Set("ETag", msc4108ETag(session.Sequence))
	h.Set("Expires", session.Expires.Time().UTC().Format(http.TimeFormat))
	h.Set("Last-Modified", session.LastModified.Time().UTC().Format(http.TimeFormat))
	h.Set("Cache-Control", "no-store, no-transform")
	h.Set("Pragma", "no-cache")
}

// readMSC4108Body reads a text/plain payload of at most 4096 bytes.
func readMSC4108Body(req *http.Request) ([]byte, string, *util.JSONResponse) {
	contentType := strings.TrimSpace(req.Header.Get("Content-Type"))
	if mediaType, _, err := mime.ParseMediaType(contentType); err != nil || mediaType != "text/plain" {
		return nil, "", &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.InvalidParam("Content-Type must be text/plain")}
	}
	if req.ContentLength > rendezvousMaxPayload {
		return nil, "", &util.JSONResponse{Code: http.StatusRequestEntityTooLarge, JSON: spec.TooLarge("Payload too large")}
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, rendezvousMaxPayload+1))
	if err != nil {
		return nil, "", &util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.BadJSON("Could not read the request body")}
	}
	if len(body) > rendezvousMaxPayload {
		return nil, "", &util.JSONResponse{Code: http.StatusRequestEntityTooLarge, JSON: spec.TooLarge("Payload too large")}
	}
	return body, contentType, nil
}

// msc4108BaseURL is the base of the session URLs handed to clients.
func (r *rendezvous) msc4108BaseURL(req *http.Request) string {
	if base := r.mscCfg.MSC3861.PublicBaseURL; base != "" {
		return strings.TrimSuffix(base, "/")
	}
	if base := r.clientCfg.Matrix.WellKnownClientName; base != "" {
		return strings.TrimSuffix(base, "/")
	}
	// This trusts the Host and X-Forwarded-Proto headers, which a reverse
	// proxy must sanitize; set public_base_url to drop that assumption.
	return requestScheme(req) + "://" + req.Host
}

// msc4108Create answers POST /org.matrix.msc4108/rendezvous. As in Synapse,
// sessions are created without an access token: the new device generating
// the QR code has none.
func (r *rendezvous) msc4108Create(w http.ResponseWriter, req *http.Request) {
	res := r.limit(req)
	if res == nil {
		res = r.limitCreate(req, nil)
	}
	if res != nil {
		writeRendezvousJSON(w, *res)
		return
	}
	data, contentType, errRes := readMSC4108Body(req)
	if errRes != nil {
		writeRendezvousJSON(w, *errRes)
		return
	}
	session, errRes := r.newSession(req, userapi.RendezvousKindMSC4108, data, contentType, nil)
	if errRes != nil {
		writeRendezvousJSON(w, *errRes)
		return
	}
	setMSC4108SessionHeaders(w, session)
	writeRendezvousJSON(w, util.JSONResponse{
		Code: http.StatusCreated,
		JSON: map[string]string{"url": r.msc4108BaseURL(req) + msc4108SessionPath + session.ID},
	})
}

// msc4108Session answers GET, PUT and DELETE on an MSC4108 session.
func (r *rendezvous) msc4108Session(w http.ResponseWriter, req *http.Request) {
	if res := r.limit(req); res != nil {
		writeRendezvousJSON(w, *res)
		return
	}
	sessionID := rendezvousVars(req)["sessionID"]
	now := r.timestamp()
	switch req.Method {
	case http.MethodGet:
		r.msc4108Get(w, req, sessionID, now)
	case http.MethodPut:
		r.msc4108Put(w, req, sessionID, now)
	case http.MethodDelete:
		deleted, err := r.userAPI.PerformRendezvousDelete(req.Context(), sessionID, userapi.RendezvousKindMSC4108, now)
		switch {
		case err != nil:
			writeRendezvousJSON(w, rendezvousInternalError(req, err, "PerformRendezvousDelete"))
		case !deleted:
			writeRendezvousJSON(w, rendezvousNotFound())
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func (r *rendezvous) msc4108Get(w http.ResponseWriter, req *http.Request, sessionID string, now spec.Timestamp) {
	if reason := unsafeFetch(req); reason != "" {
		writeRendezvousJSON(w, util.JSONResponse{Code: http.StatusForbidden, JSON: spec.Forbidden(reason)})
		return
	}
	session, err := r.userAPI.QueryRendezvousSession(req.Context(), sessionID, userapi.RendezvousKindMSC4108, now)
	if errors.Is(err, userapi.ErrRendezvousNotFound) {
		writeRendezvousJSON(w, rendezvousNotFound())
		return
	}
	if err != nil {
		writeRendezvousJSON(w, rendezvousInternalError(req, err, "QueryRendezvousSession"))
		return
	}
	setMSC4108SessionHeaders(w, session)
	if header := req.Header.Get("If-None-Match"); header != "" {
		sequences, wildcard := msc4108ETagSequences(header)
		if wildcard || slices.Contains(sequences, session.Sequence) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", session.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(session.Data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(session.Data)
}

func (r *rendezvous) msc4108Put(w http.ResponseWriter, req *http.Request, sessionID string, now spec.Timestamp) {
	data, contentType, errRes := readMSC4108Body(req)
	if errRes != nil {
		writeRendezvousJSON(w, *errRes)
		return
	}
	ifMatch := req.Header.Get("If-Match")
	if ifMatch == "" {
		writeRendezvousJSON(w, util.JSONResponse{Code: http.StatusBadRequest, JSON: spec.MissingParam("If-Match header is required")})
		return
	}
	// The compare-and-swap needs one sequence. A single ETag names it; a
	// wildcard or a list matches whichever is current, if any.
	expected := int64(-1)
	sequences, wildcard := msc4108ETagSequences(ifMatch)
	if len(sequences) == 1 && !wildcard {
		expected = sequences[0]
	} else if wildcard || len(sequences) > 1 {
		current, err := r.userAPI.QueryRendezvousSession(req.Context(), sessionID, userapi.RendezvousKindMSC4108, now)
		if errors.Is(err, userapi.ErrRendezvousNotFound) {
			writeRendezvousJSON(w, rendezvousNotFound())
			return
		}
		if err != nil {
			writeRendezvousJSON(w, rendezvousInternalError(req, err, "QueryRendezvousSession"))
			return
		}
		if wildcard || slices.Contains(sequences, current.Sequence) {
			expected = current.Sequence
		}
	}
	written, err := r.userAPI.PerformRendezvousWrite(req.Context(), &userapi.RendezvousWrite{
		ID:               sessionID,
		Kind:             userapi.RendezvousKindMSC4108,
		ExpectedSequence: expected,
		Data:             data,
		ContentType:      contentType,
		Now:              now,
	})
	switch {
	case errors.Is(err, userapi.ErrRendezvousNotFound):
		writeRendezvousJSON(w, rendezvousNotFound())
		return
	case errors.Is(err, userapi.ErrRendezvousConcurrentWrite):
		if current, queryErr := r.userAPI.QueryRendezvousSession(req.Context(), sessionID, userapi.RendezvousKindMSC4108, now); queryErr == nil {
			setMSC4108SessionHeaders(w, current)
		}
		// The 2024 MSC4108 text predates the M_CONCURRENT_WRITE errcode, so
		// it goes in an unstable field next to M_UNKNOWN, as in Synapse.
		writeRendezvousJSON(w, util.JSONResponse{
			Code: http.StatusPreconditionFailed,
			JSON: map[string]string{
				"errcode":                    string(spec.ErrorUnknown),
				"error":                      "ETag does not match",
				"org.matrix.msc4108.errcode": "M_CONCURRENT_WRITE",
			},
		})
		return
	case err != nil:
		writeRendezvousJSON(w, rendezvousInternalError(req, err, "PerformRendezvousWrite"))
		return
	}
	setMSC4108SessionHeaders(w, written)
	// Synapse sets a Content-Type on the empty answer because some reverse
	// proxies drop the ETag of a response without one.
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusAccepted)
}
