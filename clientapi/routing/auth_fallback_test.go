package routing

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	altcha "github.com/altcha-org/altcha-lib-go/v2"

	"codefloe.com/pat-s/zendrite/clientapi/auth/authtypes"
	"codefloe.com/pat-s/zendrite/setup/config"
)

func Test_AuthFallback(t *testing.T) {
	cfg := config.Zendrite{}
	cfg.Defaults(config.DefaultOpts{Generate: true, SingleDatabase: true})
	for _, useHCaptcha := range []bool{false, true} {
		for _, recaptchaEnabled := range []bool{false, true} {
			for _, wantErr := range []bool{false, true} {
				t.Run(fmt.Sprintf("useHCaptcha(%v) - recaptchaEnabled(%v) - wantErr(%v)", useHCaptcha, recaptchaEnabled, wantErr), func(t *testing.T) {
					// Set the defaults for each test
					cfg.ClientAPI.Defaults(config.DefaultOpts{Generate: true, SingleDatabase: true})
					cfg.ClientAPI.RecaptchaEnabled = recaptchaEnabled
					cfg.ClientAPI.RecaptchaPublicKey = "pub"
					cfg.ClientAPI.RecaptchaPrivateKey = "priv"
					if useHCaptcha {
						cfg.ClientAPI.RecaptchaSiteVerifyAPI = "https://hcaptcha.com/siteverify"
						cfg.ClientAPI.RecaptchaApiJsUrl = "https://js.hcaptcha.com/1/api.js"
						cfg.ClientAPI.RecaptchaFormField = "h-captcha-response"
						cfg.ClientAPI.RecaptchaSitekeyClass = "h-captcha"
					}
					cfgErrs := &config.ConfigErrors{}
					cfg.ClientAPI.Verify(cfgErrs)
					if len(*cfgErrs) > 0 {
						t.Fatalf("(hCaptcha=%v) unexpected config errors: %s", useHCaptcha, cfgErrs.Error())
					}

					req := httptest.NewRequest(http.MethodGet, "/?session=1337", nil)
					rec := httptest.NewRecorder()

					AuthFallback(rec, req, authtypes.LoginTypeRecaptcha, &cfg.ClientAPI)
					if !recaptchaEnabled {
						if rec.Code != http.StatusBadRequest {
							t.Fatalf("unexpected response code: %d, want %d", rec.Code, http.StatusBadRequest)
						}
						if rec.Body.String() != "Captcha login is disabled on this Homeserver" {
							t.Fatalf("unexpected response body: %s", rec.Body.String())
						}
					} else if !strings.Contains(rec.Body.String(), cfg.ClientAPI.RecaptchaSitekeyClass) {
						t.Fatalf("body does not contain %s: %s", cfg.ClientAPI.RecaptchaSitekeyClass, rec.Body.String())
					}

					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if wantErr {
							_, _ = w.Write([]byte(`{"success":false}`))
							return
						}
						_, _ = w.Write([]byte(`{"success":true}`))
					}))
					defer srv.Close()

					cfg.ClientAPI.RecaptchaSiteVerifyAPI = srv.URL

					// check the result after sending the captcha
					req = httptest.NewRequest(http.MethodPost, "/?session=1337", nil)
					req.Form = url.Values{}
					req.Form.Add(cfg.ClientAPI.RecaptchaFormField, "someRandomValue")
					rec = httptest.NewRecorder()
					AuthFallback(rec, req, authtypes.LoginTypeRecaptcha, &cfg.ClientAPI)
					if recaptchaEnabled {
						if !wantErr {
							if rec.Code != http.StatusOK {
								t.Fatalf("unexpected response code: %d, want %d", rec.Code, http.StatusOK)
							}
							if rec.Body.String() != successTemplate {
								t.Fatalf("unexpected response: %s, want %s", rec.Body.String(), successTemplate)
							}
						} else {
							if rec.Code != http.StatusUnauthorized {
								t.Fatalf("unexpected response code: %d, want %d", rec.Code, http.StatusUnauthorized)
							}
							wantString := "Authentication"
							if !strings.Contains(rec.Body.String(), wantString) {
								t.Fatalf("expected response to contain '%s', but didn't: %s", wantString, rec.Body.String())
							}
						}
					} else {
						if rec.Code != http.StatusBadRequest {
							t.Fatalf("unexpected response code: %d, want %d", rec.Code, http.StatusBadRequest)
						}
						if rec.Body.String() != "Captcha login is disabled on this Homeserver" {
							t.Fatalf("unexpected response: %s, want %s", rec.Body.String(), "successTemplate")
						}
					}
				})
			}
		}
	}

	t.Run("unknown fallbacks are handled correctly", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/?session=1337", nil)
		rec := httptest.NewRecorder()
		AuthFallback(rec, req, "DoesNotExist", &cfg.ClientAPI)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("unexpected http status: %d, want %d", rec.Code, http.StatusNotImplemented)
		}
	})

	t.Run("unknown methods are handled correctly", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/?session=1337", nil)
		rec := httptest.NewRecorder()
		AuthFallback(rec, req, authtypes.LoginTypeRecaptcha, &cfg.ClientAPI)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("unexpected http status: %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("missing session parameter is handled correctly", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		AuthFallback(rec, req, authtypes.LoginTypeRecaptcha, &cfg.ClientAPI)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unexpected http status: %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing session parameter is handled correctly", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		AuthFallback(rec, req, authtypes.LoginTypeRecaptcha, &cfg.ClientAPI)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unexpected http status: %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing 'response' is handled correctly", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/?session=1337", nil)
		rec := httptest.NewRecorder()
		AuthFallback(rec, req, authtypes.LoginTypeRecaptcha, &cfg.ClientAPI)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unexpected http status: %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})
}

func Test_AuthFallback_Altcha(t *testing.T) {
	const hmacKey = "test-hmac-secret"

	newAltchaCfg := func() config.ClientAPI {
		var c config.ClientAPI
		c.Defaults(config.DefaultOpts{Generate: true, SingleDatabase: true})
		c.RecaptchaEnabled = true
		c.CaptchaProvider = "altcha"
		c.AltchaHMACKey = hmacKey
		c.AltchaMaxNumber = 1000
		c.AltchaExpiry = "5m"
		return c
	}

	// solveChallenge creates a challenge and solves it, returning the
	// Base64-encoded payload that the client would submit.
	solveChallenge := func(t *testing.T, cfg *config.ClientAPI) string {
		t.Helper()
		challenge, err := newAltchaChallenge(cfg, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatalf("newAltchaChallenge: %v", err)
		}
		solution, err := altcha.SolveChallenge(altcha.SolveChallengeOptions{
			Challenge: challenge,
			DeriveKey: altcha.DeriveKeySHA(),
		})
		if err != nil {
			t.Fatalf("SolveChallenge: %v", err)
		}
		if solution == nil {
			t.Fatal("SolveChallenge returned nil")
		}
		payload := altcha.Payload{Challenge: challenge, Solution: *solution}
		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		return base64.StdEncoding.EncodeToString(payloadJSON)
	}

	t.Run("GET serves ALTCHA widget template", func(t *testing.T) {
		cfg := newAltchaCfg()
		req := httptest.NewRequest(http.MethodGet, "/?session=1337", nil)
		rec := httptest.NewRecorder()
		AuthFallback(rec, req, authtypes.LoginTypeAltcha, &cfg)
		if rec.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d, want %d", rec.Code, http.StatusOK)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "altcha-widget") {
			t.Fatalf("expected ALTCHA widget in body, got: %s", body)
		}
		// Widget v3 is the first to speak the v2 challenge protocol, and
		// takes the challenge URL in the "challenge" attribute.
		if !strings.Contains(body, "npm/altcha@3/") {
			t.Fatalf("expected ALTCHA widget v3 script in body, got: %s", body)
		}
		if !strings.Contains(body, `challenge="/_zendrite/altcha/challenge"`) {
			t.Fatalf("expected challenge attribute in body, got: %s", body)
		}
	})

	t.Run("challenge endpoint serves a signed v2 challenge", func(t *testing.T) {
		cfg := newAltchaCfg()
		rec := httptest.NewRecorder()
		ServeAltchaChallenge(rec, httptest.NewRequest(http.MethodGet, "/", nil), &cfg)
		if rec.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d, want %d", rec.Code, http.StatusOK)
		}
		var challenge altcha.Challenge
		if err := json.Unmarshal(rec.Body.Bytes(), &challenge); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		if challenge.Signature == "" {
			t.Fatal("challenge is not signed")
		}
		if challenge.Parameters.Algorithm != altchaAlgorithm {
			t.Fatalf("unexpected algorithm: %q, want %q", challenge.Parameters.Algorithm, altchaAlgorithm)
		}
		if challenge.Parameters.ExpiresAt == 0 {
			t.Fatal("challenge has no expiry")
		}
	})

	t.Run("POST with valid solution succeeds", func(t *testing.T) {
		cfg := newAltchaCfg()
		encoded := solveChallenge(t, &cfg)

		req := httptest.NewRequest(http.MethodPost, "/?session=1337", nil)
		req.Form = url.Values{}
		req.Form.Add("altcha", encoded)
		rec := httptest.NewRecorder()
		AuthFallback(rec, req, authtypes.LoginTypeAltcha, &cfg)
		if rec.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d, want %d", rec.Code, http.StatusOK)
		}
		if rec.Body.String() != successTemplate {
			t.Fatalf("unexpected response body: %s", rec.Body.String())
		}
	})

	postAltcha := func(t *testing.T, cfg *config.ClientAPI, payload string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/?session=1337", nil)
		req.Form = url.Values{}
		req.Form.Add("altcha", payload)
		rec := httptest.NewRecorder()
		AuthFallback(rec, req, authtypes.LoginTypeAltcha, cfg)
		return rec.Code
	}

	for name, payload := range map[string]string{
		"unsigned garbage": base64.StdEncoding.EncodeToString([]byte(`{"challenge":{"parameters":{"algorithm":"SHA-256","nonce":"00","salt":"00","cost":1,"keyLength":32,"keyPrefix":"00"}},"solution":{"counter":0,"derivedKey":"00"}}`)),
		"invalid base64":   "!!!not-base64!!!",
		"invalid JSON":     base64.StdEncoding.EncodeToString([]byte("{")),
	} {
		t.Run("POST with "+name+" returns 401", func(t *testing.T) {
			cfg := newAltchaCfg()
			if code := postAltcha(t, &cfg, payload); code != http.StatusUnauthorized {
				t.Fatalf("unexpected status: %d, want %d", code, http.StatusUnauthorized)
			}
		})
	}

	t.Run("POST with solution signed by another key returns 401", func(t *testing.T) {
		other := newAltchaCfg()
		other.AltchaHMACKey = "another-hmac-secret"
		encoded := solveChallenge(t, &other)

		cfg := newAltchaCfg()
		if code := postAltcha(t, &cfg, encoded); code != http.StatusUnauthorized {
			t.Fatalf("unexpected status: %d, want %d", code, http.StatusUnauthorized)
		}
	})

	t.Run("POST with wrong counter returns 401", func(t *testing.T) {
		cfg := newAltchaCfg()
		raw, err := base64.StdEncoding.DecodeString(solveChallenge(t, &cfg))
		if err != nil {
			t.Fatalf("DecodeString: %v", err)
		}
		var payload altcha.Payload
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}
		payload.Solution.Counter++
		tampered, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if code := postAltcha(t, &cfg, base64.StdEncoding.EncodeToString(tampered)); code != http.StatusUnauthorized {
			t.Fatalf("unexpected status: %d, want %d", code, http.StatusUnauthorized)
		}
	})

	t.Run("POST with expired challenge returns 401", func(t *testing.T) {
		cfg := newAltchaCfg()
		challenge, err := newAltchaChallenge(&cfg, time.Now().Add(-time.Minute))
		if err != nil {
			t.Fatalf("newAltchaChallenge: %v", err)
		}
		solution, err := altcha.SolveChallenge(altcha.SolveChallengeOptions{
			Challenge: challenge,
			DeriveKey: altcha.DeriveKeySHA(),
		})
		if err != nil || solution == nil {
			t.Fatalf("SolveChallenge: %v, %v", solution, err)
		}
		payloadJSON, err := json.Marshal(altcha.Payload{Challenge: challenge, Solution: *solution})
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if code := postAltcha(t, &cfg, base64.StdEncoding.EncodeToString(payloadJSON)); code != http.StatusUnauthorized {
			t.Fatalf("unexpected status: %d, want %d", code, http.StatusUnauthorized)
		}
	})

	t.Run("POST with empty response returns 400", func(t *testing.T) {
		cfg := newAltchaCfg()
		req := httptest.NewRequest(http.MethodPost, "/?session=1337", nil)
		req.Form = url.Values{}
		rec := httptest.NewRecorder()
		AuthFallback(rec, req, authtypes.LoginTypeAltcha, &cfg)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("unexpected status: %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})
}
