// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package routing

import (
	"context"

	"codefloe.com/pat-s/zendrite/internal/retention"
	"codefloe.com/pat-s/zendrite/roomserver/api"
	"codefloe.com/pat-s/zendrite/roomserver/types"
	"codefloe.com/pat-s/zendrite/setup/config"
)

// dropWithheldEvents removes the events that are past their room's
// retention policy and that other servers are no longer served, as if the
// purge jobs had already deleted them (MSC1763).
func dropWithheldEvents(
	ctx context.Context, cfg *config.Retention, rsAPI api.QueryLatestEventsAndStateAPI,
	events []*types.HeaderedEvent,
) ([]*types.HeaderedEvent, error) {
	rooms := retention.NewRooms(cfg, rsAPI)
	if rooms == nil || len(events) == 0 {
		return events, nil
	}
	kept := events[:0:0]
	for _, ev := range events {
		withheld, err := rooms.Withheld(ctx, ev.PDU)
		if err != nil {
			return nil, err
		}
		if !withheld {
			kept = append(kept, ev)
		}
	}
	return kept, nil
}
