// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"math"
	"math/big"
	"net/http"
	"time"

	altcha "github.com/altcha-org/altcha-lib-go/v2"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// altchaAlgorithm is the ALTCHA v2 key derivation used for challenges.
// Plain SHA-256 with a cost of one iteration keeps the proof-of-work
// profile of ALTCHA v1: the client hashes once per counter value, so
// altcha_max_number still bounds the number of attempts.
const altchaAlgorithm = "SHA-256"

// altchaDeriveKey derives the key for altchaAlgorithm, both when creating a
// challenge and when re-deriving it to verify a solution.
var altchaDeriveKey = altcha.DeriveKeySHA()

// ServeAltchaChallenge generates a proof-of-work challenge using the
// ALTCHA v2 protocol and writes it as JSON to the response.
//
// The challenge is deterministic: the server picks a secret counter in
// [0, altcha_max_number] and publishes only the prefix of the key it
// derives, so the client must try counters until it finds the same key.
func ServeAltchaChallenge(w http.ResponseWriter, req *http.Request, cfg *config.ClientAPI) {
	const defaultExpiry = 5 * time.Minute
	expiry := defaultExpiry
	if cfg.AltchaExpiry != "" {
		if d, err := time.ParseDuration(cfg.AltchaExpiry); err == nil {
			expiry = d
		}
	}
	challenge, err := newAltchaChallenge(cfg, time.Now().Add(expiry))
	if err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("failed to create ALTCHA challenge")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(challenge); err != nil {
		util.GetLogger(req.Context()).WithError(err).Error("failed to write ALTCHA challenge response")
	}
}

// newAltchaChallenge creates a signed deterministic challenge that expires
// at expiresAt.
func newAltchaChallenge(cfg *config.ClientAPI, expiresAt time.Time) (altcha.Challenge, error) {
	counter, err := altchaCounter(cfg.AltchaMaxNumber)
	if err != nil {
		return altcha.Challenge{}, err
	}
	return altcha.CreateChallenge(altcha.CreateChallengeOptions{
		Algorithm:           altchaAlgorithm,
		Cost:                1,
		Counter:             &counter,
		DeriveKey:           altchaDeriveKey,
		ExpiresAt:           &expiresAt,
		HMACSignatureSecret: cfg.AltchaHMACKey,
	})
}

// altchaCounter returns a uniformly random counter in [0, maxNumber].
// A non-positive maxNumber yields zero, the easiest possible challenge.
func altchaCounter(maxNumber int64) (int, error) {
	if maxNumber <= 0 {
		return 0, nil
	}
	n, err := rand.Int(rand.Reader, big.NewInt(min(maxNumber, math.MaxInt32)+1))
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

// validateAltcha verifies an ALTCHA v2 proof-of-work solution locally.
// The response parameter is the Base64-encoded JSON payload from the client.
func validateAltcha(cfg *config.ClientAPI, response string) error {
	if !cfg.RecaptchaEnabled {
		return ErrCaptchaDisabled
	}
	if response == "" {
		return ErrMissingResponse
	}

	// A payload that does not decode is bad client input, not a server error.
	raw, err := base64.StdEncoding.DecodeString(response)
	if err != nil {
		return ErrInvalidCaptcha
	}
	var payload altcha.Payload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ErrInvalidCaptcha
	}

	result, err := altcha.VerifySolution(altcha.VerifySolutionOptions{
		Challenge:           payload.Challenge,
		Solution:            payload.Solution,
		DeriveKey:           altchaDeriveKey,
		HMACSignatureSecret: cfg.AltchaHMACKey,
	})
	if err != nil {
		return err
	}
	if !result.Verified {
		return ErrInvalidCaptcha
	}
	return nil
}
