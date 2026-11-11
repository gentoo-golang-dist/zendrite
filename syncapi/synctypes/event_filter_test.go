// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package synctypes

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The relation type (MSC3874) and label (MSC2326) filters use Synapse's
// unstable names, and survive a round trip through a stored filter.
func TestRelationAndLabelFilterJSON(t *testing.T) {
	input := `{"room":{"timeline":{
		"org.matrix.msc3874.rel_types":["m.thread"],
		"org.matrix.msc3874.not_rel_types":["m.reference"],
		"org.matrix.labels":["#fun"],
		"org.matrix.not_labels":[]
	}}}`
	var f Filter
	require.NoError(t, json.Unmarshal([]byte(input), &f))
	timeline := f.Room.Timeline
	assert.Equal(t, &[]string{"m.thread"}, timeline.RelTypes)
	assert.Equal(t, &[]string{"m.reference"}, timeline.NotRelTypes)
	assert.Equal(t, &[]string{"#fun"}, timeline.Labels)
	assert.Equal(t, &[]string{}, timeline.NotLabels)

	out, err := json.Marshal(f)
	require.NoError(t, err)
	var again Filter
	require.NoError(t, json.Unmarshal(out, &again))
	assert.Equal(t, timeline.RelTypes, again.Room.Timeline.RelTypes)
	assert.Equal(t, timeline.Labels, again.Room.Timeline.Labels)
}

func TestMatchesRelationAndLabels(t *testing.T) {
	list := func(values ...string) *[]string { return &values }
	for _, tc := range []struct {
		name    string
		filter  *RoomEventFilter
		relType string
		labels  []string
		want    bool
	}{
		{"nil filter", nil, "m.thread", nil, true},
		{"empty filter", &RoomEventFilter{}, "", nil, true},
		{"rel_types match", &RoomEventFilter{RelTypes: list("m.thread")}, "m.thread", nil, true},
		{"rel_types other", &RoomEventFilter{RelTypes: list("m.thread")}, "m.reference", nil, false},
		{"rel_types without relation", &RoomEventFilter{RelTypes: list("m.thread")}, "", nil, false},
		{"rel_types empty", &RoomEventFilter{RelTypes: list()}, "m.thread", nil, false},
		{"not_rel_types match", &RoomEventFilter{NotRelTypes: list("m.thread")}, "m.thread", nil, false},
		{"not_rel_types without relation", &RoomEventFilter{NotRelTypes: list("m.thread")}, "", nil, true},
		{"not_rel_types wins", &RoomEventFilter{RelTypes: list("m.thread"), NotRelTypes: list("m.thread")}, "m.thread", nil, false},
		{"labels match", &RoomEventFilter{Labels: list("#a", "#b")}, "", []string{"#b"}, true},
		{"labels none", &RoomEventFilter{Labels: list("#a")}, "", nil, false},
		{"not_labels match", &RoomEventFilter{NotLabels: list("#a")}, "", []string{"#b", "#a"}, false},
		{"not_labels wins", &RoomEventFilter{Labels: list("#b"), NotLabels: list("#a")}, "", []string{"#b", "#a"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.filter.MatchesRelationAndLabels(tc.relType, tc.labels))
		})
	}
}

func TestEventRelationTypeAndLabels(t *testing.T) {
	assert.Equal(t, "m.thread", EventRelationType([]byte(`{"m.relates_to":{"rel_type":"m.thread","event_id":"$a"}}`)))
	assert.Equal(t, "", EventRelationType([]byte(`{"m.relates_to":{"rel_type":"m.thread"}}`)), "a relation needs an event ID")
	assert.Equal(t, "", EventRelationType([]byte(`{"m.relates_to":{"m.in_reply_to":{"event_id":"$a"}}}`)), "a reply is not a relation")
	assert.Equal(t, "", EventRelationType([]byte(`{"body":"hi"}`)))

	assert.Equal(t, []string{"#a", "b"}, EventLabels([]byte(`{"org.matrix.labels":["#a",1,"b","#a",null]}`)))
	assert.Nil(t, EventLabels([]byte(`{"org.matrix.labels":"#a"}`)))
	assert.Nil(t, EventLabels([]byte(`{"body":"hi"}`)))
}
