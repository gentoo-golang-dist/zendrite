// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package types

import (
	"fmt"
	"strconv"
	"strings"

	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// DefaultThreadSubscriptionsLimit is how many thread subscription changes
// the MSC4308 extension and endpoint return when the request does not say.
const DefaultThreadSubscriptionsLimit = 100

// MaxThreadSubscriptionsLimit caps the number of thread subscription changes
// in one response, as in Synapse.
const MaxThreadSubscriptionsLimit = 512

// ThreadSubscriptionsRequest configures the MSC4308 thread subscriptions
// extension.
type ThreadSubscriptionsRequest struct {
	Enabled bool `json:"enabled"`
	// Limit is how many changes to return at most, 100 by default.
	Limit *int `json:"limit,omitempty"`
}

// ThreadSubscriptionsResponse holds the latest changes to the user's thread
// subscriptions (MSC4308), keyed by room and thread root.
type ThreadSubscriptionsResponse struct {
	Subscribed   map[string]map[string]ThreadSubscribed   `json:"subscribed,omitempty"`
	Unsubscribed map[string]map[string]ThreadUnsubscribed `json:"unsubscribed,omitempty"`
	// PrevBatch is set when older changes were left out; it is the from
	// token of the /thread_subscriptions endpoint to fetch them.
	PrevBatch string `json:"prev_batch,omitempty"`
}

// ThreadSubscribed is a subscription to a thread.
type ThreadSubscribed struct {
	Automatic bool  `json:"automatic"`
	BumpStamp int64 `json:"bump_stamp"`
}

// ThreadUnsubscribed is a removed subscription to a thread.
type ThreadUnsubscribed struct {
	BumpStamp int64 `json:"bump_stamp"`
}

// NewThreadSubscriptionsResponse groups thread subscription changes, in
// ascending stream order, by room. The stream position is the bump_stamp.
// When there are limit changes, older ones may have been left out, and
// prevBatch is the token to fetch them from.
func NewThreadSubscriptionsResponse(updates []userapi.ThreadSubscriptionUpdate, limit int) (resp ThreadSubscriptionsResponse, prevBatch string) {
	for _, u := range updates {
		if u.Subscribed {
			if resp.Subscribed == nil {
				resp.Subscribed = map[string]map[string]ThreadSubscribed{}
			}
			if resp.Subscribed[u.RoomID] == nil {
				resp.Subscribed[u.RoomID] = map[string]ThreadSubscribed{}
			}
			resp.Subscribed[u.RoomID][u.ThreadRootID] = ThreadSubscribed{Automatic: u.Automatic, BumpStamp: u.StreamID}
			continue
		}
		if resp.Unsubscribed == nil {
			resp.Unsubscribed = map[string]map[string]ThreadUnsubscribed{}
		}
		if resp.Unsubscribed[u.RoomID] == nil {
			resp.Unsubscribed[u.RoomID] = map[string]ThreadUnsubscribed{}
		}
		resp.Unsubscribed[u.RoomID][u.ThreadRootID] = ThreadUnsubscribed{BumpStamp: u.StreamID}
	}
	if len(updates) > 0 && len(updates) >= limit {
		// The later bound of the next page is inclusive, and the oldest
		// change returned here is already seen.
		prevBatch = ThreadSubscriptionsToken(updates[0].StreamID - 1)
	}
	return resp, prevBatch
}

// ThreadSubscriptionsToken is a position in the thread subscriptions stream,
// formatted as Synapse does.
func ThreadSubscriptionsToken(pos int64) string {
	return fmt.Sprintf("ts%d", pos)
}

// ParseThreadSubscriptionsToken parses a ThreadSubscriptionsToken.
func ParseThreadSubscriptionsToken(token string) (int64, error) {
	rest, ok := strings.CutPrefix(token, "ts")
	if !ok {
		return 0, fmt.Errorf("thread subscriptions token must start with ts")
	}
	pos, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || pos < 0 {
		return 0, fmt.Errorf("invalid thread subscriptions token %q", token)
	}
	return pos, nil
}
