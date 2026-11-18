// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/fclient"
	"github.com/matrix-org/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/userapi/api"
)

// The application service endpoints that answer key claims (MSC3983) and
// key queries (MSC3984) for the users in their exclusive namespaces, at the
// unstable paths that Synapse calls.
const (
	appserviceKeysClaimPath = "/_matrix/app/unstable/org.matrix.msc3983/keys/claim"
	appserviceKeysQueryPath = "/_matrix/app/unstable/org.matrix.msc3984/keys/query"

	appserviceKeysTimeout      = 10 * time.Second
	appserviceKeysMaxBodyBytes = 10 << 20
)

// appserviceKeysResponse is the answer of an application service to a key
// query (MSC3984).
type appserviceKeysResponse struct {
	DeviceKeys      map[string]map[string]json.RawMessage `json:"device_keys"`
	MasterKeys      map[string]fclient.CrossSigningKey    `json:"master_keys"`
	SelfSigningKeys map[string]fclient.CrossSigningKey    `json:"self_signing_keys"`
	UserSigningKeys map[string]fclient.CrossSigningKey    `json:"user_signing_keys"`
}

// exclusiveAppservice returns the application service whose exclusive
// namespace covers a local user and that has a URL to ask, or nil.
func (a *UserInternalAPI) exclusiveAppservice(userID string) *config.ApplicationService {
	_, domain, err := gomatrixserverlib.SplitID('@', userID)
	if err != nil || !a.Config.Matrix.IsLocalServerName(domain) {
		return nil
	}
	for i := range a.AppServices {
		as := &a.AppServices[i]
		if as.URL != "" && as.OwnsNamespaceCoveringUserId(userID) {
			return as
		}
	}
	return nil
}

// askAppservice posts body to an application service endpoint and decodes
// its answer into out.
func (a *UserInternalAPI) askAppservice(ctx context.Context, as *config.ApplicationService, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, appserviceKeysTimeout)
	defer cancel()
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	target, err := url.Parse(as.RequestUrl() + path)
	if err != nil {
		return err
	}
	if a.AppserviceLegacyAuth {
		query := target.Query()
		query.Set("access_token", as.HSToken)
		target.RawQuery = query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+as.HSToken)
	req.Header.Set("Content-Type", "application/json")
	client := as.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: appserviceKeysTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return appserviceStatusError{appserviceID: as.ID, status: resp.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, appserviceKeysMaxBodyBytes)).Decode(out)
}

// appserviceStatusError is an application service answer other than 200.
type appserviceStatusError struct {
	appserviceID string
	status       int
}

func (e appserviceStatusError) Error() string {
	return fmt.Sprintf("application service %s answered %d", e.appserviceID, e.status)
}

// logAppserviceKeysError logs a failed key request to an application
// service. One that does not know the endpoint does not take part in the
// MSC, which is no reason for a warning.
func logAppserviceKeysError(ctx context.Context, as *config.ApplicationService, err error) {
	logger := util.GetLogger(ctx).WithError(err).WithField("appservice_id", as.ID)
	var statusErr appserviceStatusError
	if errors.As(err, &statusErr) && (statusErr.status == http.StatusNotFound || statusErr.status == http.StatusMethodNotAllowed) {
		logger.Debug("The application service does not answer key requests")
		return
	}
	logger.Warn("Asking the application service for keys failed")
}

// forEachAppservice runs ask for each application service at once and
// waits for all of them.
func forEachAppservice[T any](byAppservice map[*config.ApplicationService]T, ask func(*config.ApplicationService, T)) {
	var wg sync.WaitGroup
	for as, body := range byAppservice {
		wg.Go(func() { ask(as, body) })
	}
	wg.Wait()
}

// claimAppserviceKeys asks application services for one-time keys of the
// devices of their exclusive users (MSC3983), and returns the keys they gave
// and the devices left without one. An application service that fails,
// answers with something malformed or does not know the endpoint gives none.
func (a *UserInternalAPI) claimAppserviceKeys(
	ctx context.Context, query map[string]map[string]string,
) (claimed []api.OneTimeKeys, missing map[string]map[string]string) {
	missing = make(map[string]map[string]string)
	byAppservice := make(map[*config.ApplicationService]map[string]map[string][]string)
	for userID, devices := range query {
		if len(devices) == 0 {
			continue
		}
		as := a.exclusiveAppservice(userID)
		if as == nil {
			missing[userID] = devices
			continue
		}
		if byAppservice[as] == nil {
			byAppservice[as] = make(map[string]map[string][]string)
		}
		byAppservice[as][userID] = make(map[string][]string, len(devices))
		for deviceID, algorithm := range devices {
			byAppservice[as][userID][deviceID] = []string{algorithm}
		}
	}
	var mu sync.Mutex
	forEachAppservice(byAppservice, func(as *config.ApplicationService, body map[string]map[string][]string) {
		var res map[string]map[string]map[string]json.RawMessage
		if err := a.askAppservice(ctx, as, appserviceKeysClaimPath, body, &res); err != nil {
			logAppserviceKeysError(ctx, as, err)
			res = nil
		}
		mu.Lock()
		defer mu.Unlock()
		for userID, devices := range body {
			for deviceID, algorithms := range devices {
				if key, keyID := appserviceKey(res[userID][deviceID], algorithms[0]); key != nil {
					claimed = append(claimed, api.OneTimeKeys{UserID: userID, DeviceID: deviceID, KeyJSON: map[string]json.RawMessage{keyID: key}})
					continue
				}
				if missing[userID] == nil {
					missing[userID] = make(map[string]string)
				}
				missing[userID][deviceID] = algorithms[0]
			}
		}
	})
	return claimed, missing
}

// appserviceKey returns one of the keys an application service gave for a
// device that has the requested algorithm, and its key ID.
func appserviceKey(keys map[string]json.RawMessage, algorithm string) (json.RawMessage, string) {
	for keyID, key := range keys {
		if strings.HasPrefix(keyID, algorithm+":") && gjson.ValidBytes(key) && gjson.ParseBytes(key).IsObject() {
			return key, keyID
		}
	}
	return nil, ""
}

// queryAppserviceKeys asks application services for the keys of their
// exclusive users (MSC3984), and lays what they answer over res: device keys
// device by device, and cross-signing keys user by user. A device key must
// name the user and device it is given for, and keeps the display name the
// server knows if it has none. The user-signing key is only given to the
// user it belongs to. An application service that fails, answers with
// something malformed or does not know the endpoint changes nothing.
func (a *UserInternalAPI) queryAppserviceKeys(ctx context.Context, req *api.QueryKeysRequest, res *api.QueryKeysResponse) {
	byAppservice := make(map[*config.ApplicationService]map[string][]string)
	for userID, deviceIDs := range req.UserToDevices {
		as := a.exclusiveAppservice(userID)
		if as == nil {
			continue
		}
		if byAppservice[as] == nil {
			byAppservice[as] = make(map[string][]string)
		}
		if deviceIDs == nil {
			deviceIDs = []string{}
		}
		byAppservice[as][userID] = deviceIDs
	}
	answers := make(map[*config.ApplicationService]*appserviceKeysResponse, len(byAppservice))
	var mu sync.Mutex
	forEachAppservice(byAppservice, func(as *config.ApplicationService, body map[string][]string) {
		var answer appserviceKeysResponse
		if err := a.askAppservice(ctx, as, appserviceKeysQueryPath, body, &answer); err != nil {
			logAppserviceKeysError(ctx, as, err)
			return
		}
		mu.Lock()
		answers[as] = &answer
		mu.Unlock()
	})
	for as, answer := range answers {
		for userID, wanted := range byAppservice[as] {
			for deviceID, key := range answer.DeviceKeys[userID] {
				if len(wanted) > 0 && !slices.Contains(wanted, deviceID) {
					continue
				}
				parsed := gjson.ParseBytes(key)
				if !gjson.ValidBytes(key) || !parsed.IsObject() || parsed.Get("user_id").Str != userID || parsed.Get("device_id").Str != deviceID {
					continue
				}
				if res.DeviceKeys[userID] == nil {
					res.DeviceKeys[userID] = make(map[string]json.RawMessage)
				}
				if stored := gjson.GetBytes(res.DeviceKeys[userID][deviceID], "unsigned.device_display_name"); stored.Exists() && !parsed.Get("unsigned.device_display_name").Exists() {
					if withName, err := sjson.SetBytes(key, "unsigned.device_display_name", stored.Str); err == nil {
						key = withName
					}
				}
				res.DeviceKeys[userID][deviceID] = key
			}
			if key, ok := answer.MasterKeys[userID]; ok {
				res.MasterKeys[userID] = key
			}
			if key, ok := answer.SelfSigningKeys[userID]; ok {
				res.SelfSigningKeys[userID] = key
			}
			if key, ok := answer.UserSigningKeys[userID]; ok && userID == req.UserID {
				res.UserSigningKeys[userID] = key
			}
		}
	}
}
