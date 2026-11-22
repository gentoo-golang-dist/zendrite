// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"encoding/json"
	"net/http"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ownedStateTestEvent(t *testing.T, roomVersion gomatrixserverlib.RoomVersion, sender, stateKey string) gomatrixserverlib.PDU {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"type":      "com.example.test",
		"sender":    sender,
		"room_id":   "!r:a",
		"state_key": stateKey,
		"event_id":  "$e:a",
		"content":   map[string]any{},
	})
	require.NoError(t, err)
	event, err := gomatrixserverlib.MustGetRoomVersion(roomVersion).NewEventFromTrustedJSON(data, false)
	require.NoError(t, err)
	return event
}

// A refused state key that is not a valid owned state key answers 400
// M_BAD_JSON in MSC3757 rooms, as Complement's TestMSC3757OwnedState expects.
func TestMalformedOwnedStateKey(t *testing.T) {
	for _, stateKey := range []string{"@oops", "@u:a@state_key_suffix:!@#$123", "@u:"} {
		for _, v := range []gomatrixserverlib.RoomVersion{gomatrixserverlib.RoomVersionMSC3757V10, gomatrixserverlib.RoomVersionMSC3757V11} {
			res := malformedOwnedStateKey(ownedStateTestEvent(t, v, "@u:a", stateKey))
			require.NotNil(t, res, "%s %q", v, stateKey)
			assert.Equal(t, http.StatusBadRequest, res.Code)
			matrixErr, ok := res.JSON.(spec.MatrixError)
			require.True(t, ok)
			assert.Equal(t, spec.ErrorBadJSON, matrixErr.ErrCode)
		}
		// Other room versions keep answering 403.
		assert.Nil(t, malformedOwnedStateKey(ownedStateTestEvent(t, gomatrixserverlib.RoomVersionV10, "@u:a", stateKey)), stateKey)
	}
	for _, stateKey := range []string{"@u:a", "@v:a", "@v:a_suffix", "not_a_user", ""} {
		assert.Nil(t, malformedOwnedStateKey(ownedStateTestEvent(t, gomatrixserverlib.RoomVersionMSC3757V10, "@u:a", stateKey)), stateKey)
	}
}
