//go:build linux

package hachaos

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// MSC3861 delegates authentication to an OIDC provider, here a mock of the
// Matrix Authentication Service (MAS) running in the harness. Every scenario
// runs with it: the HA server's users get their access tokens from the mock,
// so every request a scenario makes is authenticated by introspection, and
// the first request of a new user provisions its account and device.

const (
	msc3861 = "msc3861"

	masClientID     = "hachaos"
	masClientSecret = "hachaos-client-secret"
	masAdminToken   = "hachaos-mas-admin-token"
	// The legacy SSO flow may only redirect to an allowlisted target, and
	// the harness never follows it.
	ssoRedirectTarget = "hachaos://sso-done"
	ssoCookieName     = "zendrite_sso_nonce"

	kindOIDCLogin  = "oidc-login"
	kindOIDCRotate = "oidc-rotate"
	// The OIDC probe signs a user in, and rotates a token, every oidcInterval.
	oidcInterval = 2 * time.Second
)

// remoteMSCs are the MSCs of the remote server. It keeps password
// registration, so it never delegates authentication.
func (s settings) remoteMSCs() []string {
	return slices.DeleteFunc(slices.Clone(s.mscs), func(msc string) bool { return msc == msc3861 })
}

func (s settings) delegatedAuth() bool { return slices.Contains(s.mscs, msc3861) }

// masGrant is what MAS knows about one access token.
type masGrant struct {
	Sub      string
	Username string
	Device   string
}

// mockMAS is the OIDC provider and MAS: discovery, token introspection, the
// authorization code exchange of the legacy SSO flow, and the admin token the
// homeserver accepts on its /_synapse/mas provisioning API. It is not behind
// a fault proxy: the scenarios qualify the homeserver, not the provider.
type mockMAS struct {
	url string
	srv *http.Server

	mu     sync.Mutex
	tokens map[string]masGrant
	codes  map[string]masGrant

	introspections atomic.Int64
}

func startMockMAS() (*mockMAS, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	m := &mockMAS{url: "http://" + l.Addr().String(), tokens: map[string]masGrant{}, codes: map[string]masGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", m.discovery)
	mux.HandleFunc("POST /oauth2/introspect", m.introspect)
	mux.HandleFunc("POST /oauth2/token", m.token)
	mux.HandleFunc("GET /oauth2/keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": []any{}})
	})
	m.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = m.srv.Serve(l) }()
	return m, nil
}

func (m *mockMAS) stop() { _ = m.srv.Close() }

// config is the candidates' MSC3861 configuration. The admin token enables
// the MAS provisioning API, so MAS usernames become localparts verbatim.
func (m *mockMAS) config() config.MSC3861Config {
	return config.MSC3861Config{
		Issuer: m.url + "/", ClientID: masClientID, ClientSecret: masClientSecret, AdminToken: masAdminToken,
		AccountManagementURL: m.url + "/account", SSORedirectAllowlist: []string{"hachaos://"},
	}
}

// issuer is the provider ID external ID mappings are stored under.
func (m *mockMAS) issuer() string { return m.url }

func writeJSON(w http.ResponseWriter, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (m *mockMAS) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                 m.url + "/",
		"authorization_endpoint": m.url + "/oauth2/auth",
		"token_endpoint":         m.url + "/oauth2/token",
		"introspection_endpoint": m.url + "/oauth2/introspect",
		"jwks_uri":               m.url + "/oauth2/keys",
	})
}

func (m *mockMAS) introspect(w http.ResponseWriter, r *http.Request) {
	m.introspections.Add(1)
	if id, secret, ok := r.BasicAuth(); !ok || id != masClientID || secret != masClientSecret {
		http.Error(w, "invalid client", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	grant, ok := m.tokens[r.PostForm.Get("token")]
	m.mu.Unlock()
	if !ok {
		writeJSON(w, map[string]any{"active": false})
		return
	}
	writeJSON(w, map[string]any{
		"active": true, "sub": grant.Sub, "username": grant.Username, "client_id": masClientID, "token_type": "access_token",
		"scope": "openid urn:matrix:client:api:* urn:matrix:client:device:" + grant.Device,
		"exp":   time.Now().Add(24 * time.Hour).Unix(),
	})
}

// token answers the authorization code exchange of the legacy SSO flow.
func (m *mockMAS) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	grant, ok := m.codes[r.PostForm.Get("code")]
	delete(m.codes, r.PostForm.Get("code"))
	m.mu.Unlock()
	if !ok {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"access_token": m.issue(grant), "refresh_token": "refresh-" + nonce(), "token_type": "Bearer", "expires_in": 3600})
}

// issue mints an access token, as MAS does on every login and refresh.
func (m *mockMAS) issue(grant masGrant) string {
	token := "mat_" + nonce() + nonce() + nonce()
	m.mu.Lock()
	m.tokens[token] = grant
	m.mu.Unlock()
	return token
}

// authorize stands in for the user signing in at MAS: it returns the
// authorization code the browser carries back to the homeserver's callback.
func (m *mockMAS) authorize(grant masGrant) string {
	code := "code-" + nonce() + nonce()
	m.mu.Lock()
	m.codes[code] = grant
	m.mu.Unlock()
	return code
}

func newDeviceID() string { return "HC" + strings.ToUpper(nonce()) }

// raw sends a request and returns the status and body, whatever the status.
func raw(ctx context.Context, c *http.Client, method, target, token string, body any, header http.Header) (int, []byte, http.Header, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, resp.Header, err
}

// noRedirects is a client that returns redirects instead of following them.
var noRedirects = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// masAPI calls the homeserver's MAS provisioning API as MAS does. Any 2xx is
// success; MAS retries anything else, so every call must be idempotent.
func masAPI(ctx context.Context, base, op string, body any) error {
	status, data, _, err := raw(ctx, noRedirects, http.MethodPost, base+"/_synapse/mas/"+op, masAdminToken, body, nil)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		mx := &mxError{Status: status}
		_ = json.Unmarshal(data, mx)
		return mx
	}
	return nil
}

func (c *mxClient) whoami(ctx context.Context) (user, device string, err error) {
	var out struct {
		UserID   string `json:"user_id"`
		DeviceID string `json:"device_id"`
	}
	err = c.do(ctx, http.MethodGet, "/_matrix/client/v3/account/whoami", nil, &out)
	return out.UserID, out.DeviceID, err
}

// errMismatch is a non-retryable error for a response that is wrong.
func errMismatch(format string, args ...any) error {
	return &mxError{Status: http.StatusConflict, Code: "HACHAOS_MISMATCH", Message: fmt.Sprintf(format, args...)}
}

// checkIdentity asserts that the client's token authenticates as its user
// and device.
func (c *mxClient) checkIdentity(ctx context.Context) error {
	user, device, err := c.whoami(ctx)
	if err != nil {
		return err
	}
	if user != c.user || device != c.device {
		return errMismatch("token authenticates as %s/%s, want %s/%s", user, device, c.user, c.device)
	}
	return nil
}

// provisionViaMAS provisions an account and device through the MAS API, as
// MAS does when a user registers and when a session starts.
func provisionViaMAS(ctx context.Context, base, localpart, device, displayName string) error {
	if err := masAPI(ctx, base, "provision_user", map[string]any{"localpart": localpart, "set_displayname": displayName}); err != nil {
		return fmt.Errorf("provision_user: %w", err)
	}
	if err := masAPI(ctx, base, "upsert_device", map[string]any{"localpart": localpart, "device_id": device}); err != nil {
		return fmt.Errorf("upsert_device: %w", err)
	}
	return nil
}

// oidcClient signs a user in through MAS: provisioning through the MAS API
// when viaMAS is set, then a first request that binds the token to its
// device. Unlike registration, every step is idempotent, so a failed attempt
// is simply repeated under the same name.
func (e *environment) oidcClient(ctx context.Context, localpart string, viaMAS bool) (*mxClient, error) {
	base := e.router.url()
	grant := masGrant{Sub: "sub-" + localpart, Username: localpart, Device: newDeviceID()}
	c := newClient(base)
	c.user, c.device, c.token = fmt.Sprintf("@%s:%s", localpart, haServerName), grant.Device, e.mas.issue(grant)
	if viaMAS {
		if err := retrying(ctx, func() error { return provisionViaMAS(ctx, base, localpart, grant.Device, localpart) }); err != nil {
			return nil, fmt.Errorf("provision %s through MAS: %w", localpart, err)
		}
	}
	if err := retrying(ctx, func() error { return c.checkIdentity(ctx) }); err != nil {
		return nil, fmt.Errorf("first request of %s: %w", localpart, err)
	}
	return c, nil
}

// localUser creates a user on the HA server: through MAS when it delegates
// authentication, otherwise by registration.
func (e *environment) localUser(ctx context.Context, name string) (*mxClient, error) {
	if e.mas == nil {
		return registerRetrying(ctx, e.router.url(), name)
	}
	return e.oidcClient(ctx, name, true)
}

// oidcProbe is MSC3861 traffic that runs alongside the workload:
//   - new users sign in, half provisioned through the MAS API first and half
//     provisioned on demand by their first request;
//   - signed-in users' tokens rotate, as MAS refreshes them, which rebinds the
//     device row to the new token.
//
// Each step is retried through takeover like the workload's writes.
type oidcProbe struct {
	w *workload
	e *environment

	mu       sync.Mutex
	logins   []*oidcLogin
	failures []string
}

// oidcLogin is one user signing in during the scenario.
type oidcLogin struct {
	client *mxClient
	grant  masGrant
	viaMAS bool
	rec    *sendRecord
}

func (l *oidcLogin) localpart() string { return l.grant.Username }

// startOIDCProbe starts the probe's traffic on the workload, so it stops
// with it.
func startOIDCProbe(w *workload) *oidcProbe {
	p := &oidcProbe{w: w, e: w.e}
	running := w.ctx
	w.run(func() { p.loginWriter(running) })
	w.run(func() { p.rotator(running) })
	return p
}

func (p *oidcProbe) failf(format string, args ...any) {
	p.mu.Lock()
	p.failures = append(p.failures, fmt.Sprintf(format, args...))
	p.mu.Unlock()
}

// noteFailure records an error the client would not retry.
func (p *oidcProbe) noteFailure(ctx context.Context, what string, err error) {
	if err != nil && ctx.Err() == nil && !retryable(err) {
		p.failf("%s: %v", what, err)
	}
}

func (p *oidcProbe) loginWriter(ctx context.Context) {
	base := p.e.router.url()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(oidcInterval):
		}
		localpart := fmt.Sprintf("oidc%d-%s", i, nonce()[:4])
		grant := masGrant{Sub: "sub-" + localpart, Username: localpart, Device: newDeviceID()}
		c := newClient(base)
		c.user, c.device, c.token = fmt.Sprintf("@%s:%s", localpart, haServerName), grant.Device, p.e.mas.issue(grant)
		login := &oidcLogin{client: c, grant: grant, viaMAS: i%2 == 0, rec: &sendRecord{Txn: p.w.nextTxn(kindOIDCLogin), Nonce: localpart, First: time.Now()}}
		p.mu.Lock()
		p.logins = append(p.logins, login)
		p.mu.Unlock()
		p.w.retry(ctx, kindOIDCLogin, login.rec, func(ctx context.Context) error {
			if login.viaMAS {
				if err := provisionViaMAS(ctx, base, localpart, grant.Device, localpart+" (MAS)"); err != nil {
					p.noteFailure(ctx, "provision "+localpart, err)
					return err
				}
			}
			err := c.checkIdentity(ctx)
			p.noteFailure(ctx, "first request of "+localpart, err)
			return err
		})
	}
}

// rotator gives a signed-in user a new access token for the same device.
func (p *oidcProbe) rotator(ctx context.Context) {
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(oidcInterval):
		}
		p.mu.Lock()
		var acked []*oidcLogin
		for _, l := range p.logins {
			if l.rec.Acked {
				acked = append(acked, l)
			}
		}
		p.mu.Unlock()
		if len(acked) == 0 {
			continue
		}
		login := acked[i%len(acked)]
		rotated := *login.client
		rotated.token = p.e.mas.issue(login.grant)
		rec := &sendRecord{Txn: p.w.nextTxn(kindOIDCRotate), Nonce: login.localpart(), First: time.Now()}
		p.w.retry(ctx, kindOIDCRotate, rec, func(ctx context.Context) error {
			err := rotated.checkIdentity(ctx)
			p.noteFailure(ctx, "rotated token of "+login.localpart(), err)
			return err
		})
		if rec.Acked {
			p.mu.Lock()
			login.client = &rotated
			p.mu.Unlock()
		}
	}
}

// oidcAccount is what the database holds for one signed-in user.
type oidcAccount struct {
	mappings    []string
	devices     []string
	displayName string
	accounts    int
}

func (e *environment) oidcAccount(ctx context.Context, sub, localpart string) (oidcAccount, error) {
	var a oidcAccount
	err := e.pg.query(ctx, func(conn *pgx.Conn) error {
		rows, err := conn.Query(ctx, "SELECT localpart FROM userapi_external_ids WHERE provider_id = $1 AND external_id = $2", e.mas.issuer(), sub)
		if err != nil {
			return err
		}
		if a.mappings, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		rows, err = conn.Query(ctx, "SELECT device_id FROM userapi_devices WHERE localpart = $1 AND server_name = $2 ORDER BY device_id", localpart, haServerName)
		if err != nil {
			return err
		}
		if a.devices, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		if err = conn.QueryRow(ctx, "SELECT count(*) FROM userapi_accounts WHERE localpart = $1 AND server_name = $2", localpart, haServerName).Scan(&a.accounts); err != nil {
			return err
		}
		var name *string
		err = conn.QueryRow(ctx, "SELECT display_name FROM userapi_profiles WHERE localpart = $1 AND server_name = $2", localpart, haServerName).Scan(&name)
		if name != nil {
			a.displayName = *name
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	return a, err
}

// verifyOIDC checks that every acknowledged sign-in left exactly one account,
// mapping and device behind, and that its latest token still authenticates
// as that user and device, whatever the scenario did in between.
func verifyOIDC(ctx context.Context, r *result, p *oidcProbe, settle time.Duration) {
	if p == nil {
		return
	}
	p.mu.Lock()
	var acked []*oidcLogin
	for _, l := range p.logins {
		if l.rec.Acked {
			acked = append(acked, l)
		}
	}
	total := len(p.logins)
	failures := slices.Clone(p.failures)
	p.mu.Unlock()
	r.notef("OIDC probe: %d of %d sign-ins acknowledged, %d introspections answered by the provider", len(acked), total, p.e.mas.introspections.Load())
	if len(acked) == 0 {
		r.failf("OIDC probe: no sign-in was acknowledged")
	}
	awaitAll(ctx, r, settle, "acknowledged OIDC sign-in still authenticating as its user and device", func() []string {
		var missing []string
		for _, l := range acked {
			p.mu.Lock()
			c := *l.client
			p.mu.Unlock()
			if err := c.checkIdentity(ctx); err != nil {
				missing = append(missing, fmt.Sprintf("%s (%v)", l.localpart(), err))
			}
		}
		return missing
	})
	for _, l := range acked {
		a, err := p.e.oidcAccount(ctx, l.grant.Sub, l.localpart())
		switch {
		case err != nil:
			r.failf("OIDC probe: read the account of %s: %v", l.localpart(), err)
		case a.accounts != 1:
			r.failf("OIDC probe: %s has %d accounts", l.localpart(), a.accounts)
		case len(a.mappings) != 1 || a.mappings[0] != l.localpart():
			r.failf("OIDC probe: subject %s maps to %v, want exactly %s", l.grant.Sub, a.mappings, l.localpart())
		case len(a.devices) != 1 || a.devices[0] != l.grant.Device:
			r.failf("OIDC probe: %s has devices %v, want exactly %s", l.localpart(), a.devices, l.grant.Device)
		case l.viaMAS && a.displayName != l.localpart()+" (MAS)":
			r.failf("OIDC probe: %s has display name %q, want the one MAS provisioned", l.localpart(), a.displayName)
		}
	}
	for _, f := range failures {
		r.failf("OIDC probe: %s", f)
	}
}

// crossSigningUpload is the answer to a cross-signing key upload.
type crossSigningUpload struct {
	status  int
	session string
	stages  []string
}

func newMasterKey() string {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return base64.RawStdEncoding.EncodeToString(public)
}

// uploadMasterKey uploads key as c's cross-signing master key, completing
// user-interactive authentication with session when it is set.
func (c *mxClient) uploadMasterKey(ctx context.Context, key, session string) (crossSigningUpload, error) {
	body := map[string]any{"master_key": map[string]any{"user_id": c.user, "usage": []string{"master"}, "keys": map[string]string{"ed25519:" + key: key}}}
	if session != "" {
		body["auth"] = map[string]any{"type": "m.oauth", "session": session}
	}
	status, data, _, err := raw(ctx, c.http, http.MethodPost, c.base+"/_matrix/client/v3/keys/device_signing/upload", c.token, body, nil)
	if err != nil {
		return crossSigningUpload{}, err
	}
	out := crossSigningUpload{status: status}
	switch {
	case status >= http.StatusInternalServerError:
		return out, &mxError{Status: status, Message: string(data)}
	case status == http.StatusUnauthorized:
		var uia struct {
			Session string `json:"session"`
			Flows   []struct {
				Stages []string `json:"stages"`
			} `json:"flows"`
		}
		if err = json.Unmarshal(data, &uia); err != nil {
			return out, err
		}
		out.session = uia.Session
		for _, f := range uia.Flows {
			out.stages = append(out.stages, f.Stages...)
		}
	case status != http.StatusOK:
		return out, &mxError{Status: status, Message: string(data)}
	}
	return out, nil
}

func (c *mxClient) masterKey(ctx context.Context) (string, error) {
	var out struct {
		MasterKeys map[string]struct {
			Keys map[string]string `json:"keys"`
		} `json:"master_keys"`
	}
	if err := c.do(ctx, http.MethodPost, "/_matrix/client/v3/keys/query", map[string]any{"device_keys": map[string]any{c.user: []string{}}}, &out); err != nil {
		return "", err
	}
	for _, key := range out.MasterKeys[c.user].Keys {
		return key, nil
	}
	return "", nil
}

// ssoLogin is a legacy SSO login in progress.
type ssoLogin struct {
	state  string
	cookie string
	device string
}

// beginSSO starts a legacy SSO login as Element Web does.
func beginSSO(ctx context.Context, base string) (ssoLogin, error) {
	status, data, header, err := raw(ctx, noRedirects, http.MethodGet, base+"/_matrix/client/v3/login/sso/redirect?redirectUrl="+url.QueryEscape(ssoRedirectTarget), "", nil, nil)
	if err != nil {
		return ssoLogin{}, err
	}
	if status != http.StatusFound {
		return ssoLogin{}, &mxError{Status: status, Message: string(data)}
	}
	location, err := url.Parse(header.Get("Location"))
	if err != nil {
		return ssoLogin{}, err
	}
	login := ssoLogin{state: location.Query().Get("state")}
	for _, scope := range strings.Fields(location.Query().Get("scope")) {
		if device, ok := strings.CutPrefix(scope, "urn:matrix:client:device:"); ok {
			login.device = device
		}
	}
	for _, cookie := range (&http.Response{Header: header}).Cookies() {
		if cookie.Name == ssoCookieName {
			login.cookie = cookie.Value
		}
	}
	if login.state == "" || login.cookie == "" || login.device == "" {
		return ssoLogin{}, errMismatch("incomplete SSO redirect to %s", location)
	}
	return login, nil
}

// completeSSO brings the browser back from MAS and returns the login token.
func completeSSO(ctx context.Context, base string, login ssoLogin, code string) (string, error) {
	q := url.Values{"state": {login.state}, "code": {code}}
	status, data, header, err := raw(ctx, noRedirects, http.MethodGet, base+"/_matrix/client/v3/login/sso/callback?"+q.Encode(), "", nil,
		http.Header{"Cookie": {ssoCookieName + "=" + login.cookie}})
	if err != nil {
		return "", err
	}
	if status != http.StatusFound {
		return "", &mxError{Status: status, Message: string(data)}
	}
	location, err := url.Parse(header.Get("Location"))
	if err != nil {
		return "", err
	}
	return location.Query().Get("loginToken"), nil
}

// redeemLoginToken posts the login token as the client does, returning the
// status and the access token on success.
func redeemLoginToken(ctx context.Context, base, loginToken string) (int, string, error) {
	status, data, _, err := raw(ctx, noRedirects, http.MethodPost, base+"/_matrix/client/v3/login", "", map[string]any{"type": "m.login.token", "token": loginToken}, nil)
	if err != nil {
		return 0, "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(data, &out)
	return status, out.AccessToken, nil
}

// untilAnswered repeats a request while it fails as a takeover makes
// requests fail, and returns the first real answer.
func untilAnswered(ctx context.Context, op func() error) error {
	return retrying(ctx, func() error {
		err := op()
		var mx *mxError
		if err != nil && (!errors.As(err, &mx) || mx.Status >= http.StatusInternalServerError || mx.Status == http.StatusTooManyRequests) {
			return err
		}
		if err != nil {
			return errPermanent(err)
		}
		return nil
	})
}

// msc3861TakeoverScenario freezes the owner while MSC3861 holds state that
// only that process knows: a cross-signing reset MAS confirmed to it, the
// provider-issued tokens behind an SSO login token it handed out, and MAS
// provisioning calls it acknowledged. The successor must refuse what it never
// saw rather than honor it, MAS's and the client's retries must then
// succeed, and the frozen owner must not act on its memory when it resumes.
func msc3861TakeoverScenario() scenario {
	return scenario{
		name:        "msc3861-takeover-intermediates",
		description: "With delegated authentication, the active candidate is frozen while a confirmed cross-signing reset, a legacy SSO login token and MAS provisioning are in flight, and replaced. The successor refuses what only the old owner knew, retried flows succeed, and the resumed old owner acts on none of it.",
		tiers:       allTiers,
		run: func(ctx context.Context, s *run) error {
			e := s.e
			if e.mas == nil {
				return errors.New("the scenario needs MSC3861; unset HACHAOS_MSCS")
			}
			base := e.router.url()

			// A cross-signing reset the user confirmed at MAS, which told
			// the owner, before the client resubmitted the upload.
			carol, err := e.oidcClient(ctx, "carol-"+nonce()[:6], true)
			if err != nil {
				return err
			}
			original, replacement := newMasterKey(), newMasterKey()
			if err = untilAnswered(ctx, func() error { _, err := carol.uploadMasterKey(ctx, original, ""); return err }); err != nil {
				return fmt.Errorf("upload the first master key: %w", err)
			}
			var challenge crossSigningUpload
			if err = untilAnswered(ctx, func() (err error) { challenge, err = carol.uploadMasterKey(ctx, replacement, ""); return err }); err != nil {
				return fmt.Errorf("request a master key replacement: %w", err)
			}
			if challenge.status != http.StatusUnauthorized || !slices.Contains(challenge.stages, "m.oauth") {
				return fmt.Errorf("replacing the master key was answered %d with stages %v, want an m.oauth challenge", challenge.status, challenge.stages)
			}
			if err = untilAnswered(ctx, func() error {
				return masAPI(ctx, base, "allow_cross_signing_reset", map[string]any{"localpart": strings.TrimPrefix(strings.Split(carol.user, ":")[0], "@")})
			}); err != nil {
				return fmt.Errorf("confirm the reset: %w", err)
			}

			// A legacy SSO login whose login token was handed out, not yet
			// redeemed.
			dave := masGrant{Sub: "sub-dave-" + nonce(), Username: "dave-" + nonce()[:6]}
			var sso ssoLogin
			var loginToken string
			if err = untilAnswered(ctx, func() (err error) {
				if sso, err = beginSSO(ctx, base); err != nil {
					return err
				}
				dave.Device = sso.device
				loginToken, err = completeSSO(ctx, base, sso, e.mas.authorize(dave))
				return err
			}); err != nil {
				return fmt.Errorf("complete an SSO login: %w", err)
			}

			// MAS provisioned a user and device; MAS retries it against the
			// successor as if it never saw the acknowledgement.
			dora := masGrant{Sub: "sub-dora-" + nonce(), Username: "dora-" + nonce()[:6], Device: newDeviceID()}
			if err = untilAnswered(ctx, func() error { return provisionViaMAS(ctx, base, dora.Username, dora.Device, "Dora") }); err != nil {
				return err
			}
			// A user whose first request reaches the successor.
			erin := masGrant{Sub: "sub-erin-" + nonce(), Username: "erin-" + nonce()[:6], Device: newDeviceID()}
			erinClient := newClient(base)
			erinClient.user, erinClient.device, erinClient.token = fmt.Sprintf("@%s:%s", erin.Username, haServerName), erin.Device, e.mas.issue(erin)

			leader, err := s.active()
			if err != nil {
				return err
			}
			s.faultf(leader.name, "SIGSTOP active candidate with MSC3861 state in flight")
			if err = s.pause(leader); err != nil {
				return err
			}
			if err = s.successor(leader, 3*s.e.timings.Lease+time.Minute); err != nil {
				_ = s.resume(leader)
				return err
			}

			// The successor never received the confirmation: the upload
			// must be challenged again, not completed.
			var resubmitted crossSigningUpload
			if err = untilAnswered(ctx, func() (err error) {
				resubmitted, err = carol.uploadMasterKey(ctx, replacement, challenge.session)
				return err
			}); err != nil {
				_ = s.resume(leader)
				return fmt.Errorf("resubmit the replacement to the successor: %w", err)
			}
			if resubmitted.status != http.StatusUnauthorized {
				s.r.failf("the successor answered %d to a cross-signing replacement whose confirmation only the old owner received", resubmitted.status)
			} else {
				s.r.notef("the successor challenged the resubmitted cross-signing replacement again")
			}

			// The login token's provider tokens were in the old owner's
			// memory: the successor refuses the token.
			status, _, err := redeemLoginToken(ctx, base, loginToken)
			switch {
			case err != nil:
				s.r.failf("redeem the SSO login token at the successor: %v", err)
			case status == http.StatusOK:
				s.r.failf("the successor redeemed an SSO login token whose provider tokens only the old owner held")
			case status != http.StatusForbidden:
				s.r.failf("the successor answered %d to an SSO login token it cannot redeem, want 403", status)
			}

			// MAS retries; the user signs in.
			if err = untilAnswered(ctx, func() error { return provisionViaMAS(ctx, base, dora.Username, dora.Device, "Dora") }); err != nil {
				s.r.failf("MAS retried provisioning against the successor: %v", err)
			}
			doraClient := newClient(base)
			doraClient.user, doraClient.device, doraClient.token = fmt.Sprintf("@%s:%s", dora.Username, haServerName), dora.Device, e.mas.issue(dora)
			if err = untilAnswered(ctx, func() error { return doraClient.checkIdentity(ctx) }); err != nil {
				s.r.failf("first request of a user MAS provisioned before the takeover: %v", err)
			}
			if err = untilAnswered(ctx, func() error { return erinClient.checkIdentity(ctx) }); err != nil {
				s.r.failf("first request of a new user at the successor: %v", err)
			}

			// The resumed old owner still holds the confirmation and the
			// provider tokens, and must act on neither.
			if err = s.resume(leader); err != nil {
				return err
			}
			direct := newClient(leader.httpURL()).withToken(carol)
			direct.http.Timeout = 10 * time.Second
			if upload, err := direct.uploadMasterKey(ctx, replacement, challenge.session); err == nil && upload.status == http.StatusOK {
				s.r.failf("%s completed a cross-signing replacement after its successor was promoted", leader.name)
			}
			if status, _, err := redeemLoginToken(ctx, leader.httpURL(), loginToken); err == nil && status == http.StatusOK {
				s.r.failf("%s redeemed an SSO login token after its successor was promoted", leader.name)
			}
			s.expectRefused(ctx, leader, 5*time.Second)

			// The user confirms again, which MAS reports to the successor,
			// and the client resubmits.
			if err = untilAnswered(ctx, func() error {
				return masAPI(ctx, base, "allow_cross_signing_reset", map[string]any{"localpart": strings.TrimPrefix(strings.Split(carol.user, ":")[0], "@")})
			}); err != nil {
				s.r.failf("confirm the reset again at the successor: %v", err)
			}
			var completed crossSigningUpload
			if err = untilAnswered(ctx, func() (err error) {
				completed, err = carol.uploadMasterKey(ctx, replacement, resubmitted.session)
				return err
			}); err != nil || completed.status != http.StatusOK {
				s.r.failf("replace the master key after confirming again: %d, %v", completed.status, err)
			}
			var key string
			if err = untilAnswered(ctx, func() (err error) { key, err = carol.masterKey(ctx); return err }); err != nil || key != replacement {
				s.r.failf("the master key after the reset is %q (%v), want the replacement", key, err)
			}

			// The user starts the SSO login again.
			if err = untilAnswered(ctx, func() (err error) {
				if sso, err = beginSSO(ctx, base); err != nil {
					return err
				}
				dave.Device = sso.device
				if loginToken, err = completeSSO(ctx, base, sso, e.mas.authorize(dave)); err != nil {
					return err
				}
				status, token, err := redeemLoginToken(ctx, base, loginToken)
				if err != nil {
					return err
				}
				if status != http.StatusOK {
					return &mxError{Status: status, Message: "redeem the login token"}
				}
				daveClient := newClient(base)
				daveClient.user, daveClient.device, daveClient.token = fmt.Sprintf("@%s:%s", dave.Username, haServerName), dave.Device, token
				return daveClient.checkIdentity(ctx)
			}); err != nil {
				s.r.failf("sign in again through SSO at the successor: %v", err)
			}

			for _, g := range []masGrant{dora, erin} {
				a, err := e.oidcAccount(ctx, g.Sub, g.Username)
				switch {
				case err != nil:
					s.r.failf("read the account of %s: %v", g.Username, err)
				case a.accounts != 1 || len(a.mappings) != 1 || len(a.devices) != 1 || a.devices[0] != g.Device:
					s.r.failf("%s has %d accounts, mappings %v and devices %v, want one of each", g.Username, a.accounts, a.mappings, a.devices)
				case g.Username == dora.Username && a.displayName != "Dora":
					s.r.failf("%s has display name %q, want the one MAS provisioned", g.Username, a.displayName)
				}
			}
			return nil
		},
	}
}
