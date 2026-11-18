// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package eventutil

import (
	"encoding/json"
	"strings"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
)

func TestEDUSize(t *testing.T) {
	size, err := EDUSize("m.typing", "a.example", json.RawMessage(`{ "user_id" : "@a:b" }`))
	require.NoError(t, err)
	// Canonical JSON drops the whitespace.
	require.Equal(t, len(`{"content":{"user_id":"@a:b"},"edu_type":"m.typing","origin":"a.example"}`), size)

	overhead := len(`{"content":{"a":""},"edu_type":"x","origin":"o"}`)
	require.False(t, EDUTooLarge("x", "o", map[string]string{"a": strings.Repeat("a", MaxEDUSize-overhead)}))
	require.True(t, EDUTooLarge("x", "o", map[string]string{"a": strings.Repeat("a", MaxEDUSize-overhead+1)}))
	// Raw content is measured the same way, past the shortcut for small content.
	require.True(t, EDUTooLarge("x", "o", spec.RawJSON(`{"a":"`+strings.Repeat("a", MaxEDUSize-overhead+1)+`"}`)))
	require.False(t, EDUTooLarge("x", "o", spec.RawJSON(`{"a":"`+strings.Repeat("a", MaxEDUSize-overhead)+`"}`)))
	// Content that cannot be encoded is left to whatever handles the EDU.
	require.False(t, EDUTooLarge("x", "o", spec.RawJSON(`{not json`)))
	require.False(t, EDUTooLarge("x", "o", spec.RawJSON(nil)))
}
