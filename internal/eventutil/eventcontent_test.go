// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package eventutil

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlainTextTopic(t *testing.T) {
	tests := map[string]struct {
		content string
		want    string
	}{
		"legacy only":       {`{"topic":"plain"}`, "plain"},
		"no topic":          {`{}`, ""},
		"null topic":        {`{"topic":null}`, ""},
		"non-string topic":  {`{"topic":42}`, ""},
		"implicit mimetype": {`{"topic":"old","m.topic":{"m.text":[{"body":"new"}]}}`, "new"},
		"plain after html":  {`{"topic":"old","m.topic":{"m.text":[{"body":"<b>x</b>","mimetype":"text/html"},{"body":"x","mimetype":"text/plain"}]}}`, "x"},
		"html only":         {`{"topic":"old","m.topic":{"m.text":[{"body":"<b>x</b>","mimetype":"text/html"}]}}`, "old"},
		"empty block":       {`{"topic":"old","m.topic":{"m.text":[]}}`, "old"},
		"malformed block":   {`{"topic":"old","m.topic":"nope"}`, "old"},
		"non-string body":   {`{"topic":"old","m.topic":{"m.text":[{"body":1}]}}`, "old"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, PlainTextTopic([]byte(tc.content)))
		})
	}
}

func TestNewTopicContent(t *testing.T) {
	content, err := json.Marshal(NewTopicContent("hello"))
	require.NoError(t, err)
	require.JSONEq(t, `{"topic":"hello","m.topic":{"m.text":[{"body":"hello","mimetype":"text/plain"}]}}`, string(content))
	require.Equal(t, "hello", PlainTextTopic(content))
}
