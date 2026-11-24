// Copyright 2026 New Vector Ltd.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"net/http"
	"testing"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateMessageContent(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		content   map[string]any
		wantErr   string
	}{
		{name: "text message", eventType: "m.room.message", content: map[string]any{"msgtype": "m.text", "body": "hi"}},
		{name: "empty body", eventType: "m.room.message", content: map[string]any{"msgtype": "m.notice", "body": ""}},
		{name: "unknown msgtype", eventType: "m.room.message", content: map[string]any{"msgtype": "org.example.custom", "body": "hi"}},
		{name: "other event type", eventType: "m.reaction", content: map[string]any{}},
		{name: "missing body", eventType: "m.room.message", content: map[string]any{"msgtype": "m.notice"}, wantErr: "'body' not in content"},
		{name: "missing msgtype", eventType: "m.room.message", content: map[string]any{"body": "hi"}, wantErr: "'msgtype' not in content"},
		{name: "body not a string", eventType: "m.room.message", content: map[string]any{"msgtype": "m.text", "body": 1.0}, wantErr: "'body' not a string type"},
		{name: "msgtype null", eventType: "m.room.message", content: map[string]any{"msgtype": nil, "body": "hi"}, wantErr: "'msgtype' not a string type"},
		{name: "empty content", eventType: "m.room.message", content: map[string]any{}, wantErr: "'body' not in content"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := validateMessageContent(tc.eventType, tc.content)
			if tc.wantErr == "" {
				assert.Nil(t, res)
				return
			}
			require.NotNil(t, res)
			assert.Equal(t, http.StatusBadRequest, res.Code)
			matrixErr, ok := res.JSON.(spec.MatrixError)
			require.True(t, ok, "JSON is %T", res.JSON)
			assert.Equal(t, spec.ErrorBadJSON, matrixErr.ErrCode)
			assert.Equal(t, tc.wantErr, matrixErr.Err)
		})
	}
}
