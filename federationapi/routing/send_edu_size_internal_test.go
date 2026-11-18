// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"strings"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/require"
)

// An incoming EDU larger than 64 KiB is dropped, and the others kept
// (MSC4514).
func TestDropOversizedEDUs(t *testing.T) {
	small := gomatrixserverlib.EDU{Type: spec.MTyping, Content: spec.RawJSON(`{"room_id":"!r:a","user_id":"@u:a","typing":true}`)}
	large := gomatrixserverlib.EDU{Type: spec.MDirectToDevice, Content: spec.RawJSON(`{"padding":"` + strings.Repeat("a", 65536) + `"}`)}
	kept := dropOversizedEDUs(t.Context(), "remote", []gomatrixserverlib.EDU{small, large, small})
	require.Equal(t, []gomatrixserverlib.EDU{small, small}, kept)
}
