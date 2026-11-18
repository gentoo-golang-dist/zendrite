// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package authtypes

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProfileSizeOfStoredFields checks that profile fields stored before
// request bodies were checked for ambiguous JSON (MSC4458) can still be
// measured, so the profile stays editable.
func TestProfileSizeOfStoredFields(t *testing.T) {
	p := Profile{DisplayName: "alice", Fields: map[string]json.RawMessage{
		"x": json.RawMessage(`{"a":1,"a":2}`),
		"y": json.RawMessage(`"\ud83d"`),
	}}
	size, err := p.Size()
	require.NoError(t, err)
	assert.Positive(t, size)
}
