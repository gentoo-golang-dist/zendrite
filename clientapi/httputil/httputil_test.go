// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package httputil

import (
	"encoding/json"
	"net/http"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnmarshalJSONRejectsAmbiguousJSON checks that request bodies that
// other servers would reject when relayed to them are refused up front
// (MSC4458).
func TestUnmarshalJSONRejectsAmbiguousJSON(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate key":        `{"messages":{"@a:x":{"DEV":{"a":1,"a":2}}}}`,
		"escaped duplicate":    `{"body":"x","body":"y"}`,
		"unpaired surrogate":   `{"body":"\ud83d"}`,
		"lone low surrogate":   `{"body":"\ude00 x"}`,
		"surrogate as UTF-8":   "{\"body\":\"\xed\xa0\xbd\"}",
		"nested duplicate key": `[{"k":1,"k":1}]`,
	} {
		t.Run(name, func(t *testing.T) {
			var v any
			res := UnmarshalJSON([]byte(body), &v)
			require.NotNil(t, res)
			assert.Equal(t, http.StatusBadRequest, res.Code)
			var matrixErr spec.MatrixError
			raw, err := json.Marshal(res.JSON)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &matrixErr))
			assert.Contains(t, []spec.MatrixErrorCode{spec.ErrorBadJSON, spec.ErrorNotJSON}, matrixErr.ErrCode)
		})
	}

	var v map[string]any
	assert.Nil(t, UnmarshalJSON([]byte(`{"body":"😀","n":1.5}`), &v), "valid pairs and floats are fine")
	assert.Equal(t, "\U0001F600", v["body"])
}
