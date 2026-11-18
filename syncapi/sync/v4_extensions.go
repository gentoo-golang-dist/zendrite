// Copyright 2025 Jackmaninov
// Copyright 2025 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sync

import (
	"context"
	"encoding/json"
	"fmt"

	"codefloe.com/pat-s/gomatrixserverlib"
	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/syncapi/internal"
	"codefloe.com/pat-s/zendrite/syncapi/storage"
	"codefloe.com/pat-s/zendrite/syncapi/streams"
	"codefloe.com/pat-s/zendrite/syncapi/synctypes"
	"codefloe.com/pat-s/zendrite/syncapi/types"
	userapi "codefloe.com/pat-s/zendrite/userapi/api"
)

// findRelevantRoomIDsForExtension handles the reserved `lists`/`rooms` keys for extensions.
// Extensions should only return results for rooms in the Sliding Sync response. This matches up
// the requested rooms/lists with the actual lists/rooms in the Sliding Sync response.
//
// Behavior (MSC3959, MSC3960, MSC3961):
//
//	nil (omitted)              // Default: Process all rooms (wildcard behavior)
//	{"lists": []}              // Explicitly process no lists
//	{"lists": ["rooms", "dms"]} // Process only specified lists
//	{"lists": ["*"]}           // Process all lists (explicit wildcard)
//	{"rooms": []}              // Explicitly process no room subscriptions
//	{"rooms": ["!a:b", "!c:d"]} // Process only specified room subscriptions
//	{"rooms": ["*"]}           // Process all room subscriptions (explicit wildcard)
//
// Args:
//
//	requestedLists: The `lists` from the extension request (nil = default wildcard)
//	requestedRooms: The `rooms` from the extension request (nil = default wildcard)
//	actualLists: The actual lists from the Sliding Sync response
//	actualRoomSubscriptions: The actual room subscriptions from the Sliding Sync request
//
// Returns: Set of room IDs to process for this extension.
func findRelevantRoomIDsForExtension(
	requestedLists []string,
	requestedRooms []string,
	actualLists map[string]types.SlidingList,
	actualRoomSubscriptions map[string]bool,
) map[string]bool {
	relevantRoomIDs := make(map[string]bool)

	// Handle rooms parameter
	if requestedRooms != nil {
		// Explicitly provided (could be empty array)
		if len(requestedRooms) == 0 {
			// Empty array [] = explicitly process no rooms
			// Continue to check lists parameter
		} else {
			for _, roomID := range requestedRooms {
				// Wildcard means process all room subscriptions
				if roomID == "*" {
					for roomID := range actualRoomSubscriptions {
						relevantRoomIDs[roomID] = true
					}
					break
				}

				// Specific room - only include if in actual subscriptions
				if actualRoomSubscriptions[roomID] {
					relevantRoomIDs[roomID] = true
				}
			}
		}
	} else {
		// nil (omitted) = default to wildcard behavior (all room subscriptions)
		for roomID := range actualRoomSubscriptions {
			relevantRoomIDs[roomID] = true
		}
	}

	// Handle lists parameter
	if requestedLists != nil {
		// Explicitly provided (could be empty array)
		if len(requestedLists) == 0 {
			// Empty array [] = explicitly process no lists
			// relevantRoomIDs already populated from rooms parameter above
		} else {
			for _, listKey := range requestedLists {
				// Wildcard means process all lists
				if listKey == "*" {
					for _, list := range actualLists {
						for _, roomID := range list.RoomIDs {
							relevantRoomIDs[roomID] = true
						}
					}
					break
				}

				// Specific list - only include if it exists
				if list, exists := actualLists[listKey]; exists {
					for _, roomID := range list.RoomIDs {
						relevantRoomIDs[roomID] = true
					}
				}
			}
		}
	} else {
		// nil (omitted) = default to wildcard behavior (all lists)
		for _, list := range actualLists {
			for _, roomID := range list.RoomIDs {
				relevantRoomIDs[roomID] = true
			}
		}
	}

	return relevantRoomIDs
}

// ProcessExtensions handles all extension requests and populates the response
// Phase 9: Implements to_device, e2ee (MSC3884), account_data, receipts, typing extensions
//
// Reference: /tmp/phase9_plan.md, /tmp/msc3884_research.md, /tmp/matrix_js_sdk_findings.md
//
// Processing order (from matrix-js-sdk):
//   - PreProcess: to_device, e2ee (before room data processing)
//   - PostProcess: account_data, receipts, typing (after room data processing)
//
// For now, we process all extensions together. Future optimization: split by PreProcess/PostProcess.
//
// Once ctx is done it stops and returns the context's error rather than an
// extension failure per remaining extension: the snapshot is rolled back with
// the request, so later reads would only fail with "transaction has already
// been committed or rolled back".
//
// ResponselLists: The actual lists from the sliding sync response (for extension filtering).
// RoomSubscriptions: The actual room subscriptions from the sliding sync request (for extension filtering).
func (rp *RequestPool) ProcessExtensions(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	connState *V4ConnectionState, // For per-room extension state (e.g., account_data); may be nil
	req *types.SlidingSyncRequest,
	userID string,
	deviceID string,
	connectionKey int64, // For per-connection extension state (e.g., receipts)
	fromPos *types.StreamingToken, // nil for initial sync
	toPos types.StreamingToken,
	responseLists map[string]types.SlidingList, // Actual lists in the response
	roomSubscriptions map[string]bool, // Actual room subscriptions from request
	responseRooms map[string]types.SlidingRoomData, // Rooms in the response, whose timelines sticky events skip
) (*types.ExtensionResponse, types.StreamingToken, types.ReceiptDelivery, error) {
	// Return empty response if no extensions requested
	if req.Extensions == nil {
		return &types.ExtensionResponse{}, toPos, types.ReceiptDelivery{}, nil
	}

	resp := &types.ExtensionResponse{Format: req.ExtensionFormat}
	isInitialSync := (fromPos == nil)

	// Track updated positions from extensions (like v3 sync stream providers)
	updatedPos := toPos

	if err := ctx.Err(); err != nil {
		return nil, toPos, types.ReceiptDelivery{}, err
	}

	// Process to_device extension (PreProcess)
	if req.Extensions.ToDevice != nil && req.Extensions.ToDevice.Enabled {
		toDeviceResp, toDevicePos, err := rp.processToDeviceExtension(ctx, snapshot, userID, deviceID, req.Extensions.ToDevice, fromPos, toPos)
		if err != nil {
			if ctx.Err() != nil {
				return nil, toPos, types.ReceiptDelivery{}, ctx.Err()
			}
			logrus.WithError(err).Error("Failed to process to_device extension")
			// Continue anyway - extensions are optional
		} else {
			resp.ToDevice = toDeviceResp
			// A response cut off at limit must not move the connection
			// position past its last message: clients without since resume
			// from it, and the next long poll returns at once while
			// messages remain.
			if toDevicePos < updatedPos.SendToDevicePosition {
				updatedPos.SendToDevicePosition = toDevicePos
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, toPos, types.ReceiptDelivery{}, err
	}

	// Process e2ee extension (PreProcess, MSC3884)
	if req.Extensions.E2EE != nil && req.Extensions.E2EE.Enabled {
		e2eeResp, err := rp.processE2EEExtension(ctx, snapshot, userID, deviceID, isInitialSync, fromPos, toPos)
		if err != nil {
			if ctx.Err() != nil {
				return nil, toPos, types.ReceiptDelivery{}, ctx.Err()
			}
			logrus.WithError(err).Error("Failed to process e2ee extension")
			// Continue anyway - extensions are optional
		} else {
			resp.E2EE = e2eeResp
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, toPos, types.ReceiptDelivery{}, err
	}

	// Process account_data extension (PostProcess)
	if req.Extensions.AccountData != nil && req.Extensions.AccountData.Enabled {
		accountDataResp, accountDataLastPos, err := rp.processAccountDataExtension(ctx, snapshot, connState, userID, req.Extensions.AccountData.Lists, req.Extensions.AccountData.Rooms, fromPos, toPos, responseLists, roomSubscriptions)
		if err != nil {
			if ctx.Err() != nil {
				return nil, toPos, types.ReceiptDelivery{}, ctx.Err()
			}
			logrus.WithError(err).Error("Failed to process account_data extension")
			// Return empty response on error - clients expect the field to be present with proper structure
			resp.AccountData = &types.AccountDataResponse{
				Global: []synctypes.ClientEvent{},
				Rooms:  make(map[string][]synctypes.ClientEvent),
			}
		} else {
			resp.AccountData = accountDataResp
			// Update account data position based on what was actually returned (v4 sync fix)
			// This ensures the position token matches the account data delivered to the client
			logrus.WithFields(logrus.Fields{
				"accountDataLastPos":             accountDataLastPos,
				"updatedPos.AccountDataPosition": updatedPos.AccountDataPosition,
				"toPos.AccountDataPosition":      toPos.AccountDataPosition,
			}).Debug("[ACCOUNT_DATA] Checking position update")
			if accountDataLastPos > updatedPos.AccountDataPosition {
				updatedPos.AccountDataPosition = accountDataLastPos
				logrus.WithFields(logrus.Fields{
					"old_pos": toPos.AccountDataPosition,
					"new_pos": accountDataLastPos,
				}).Info("[ACCOUNT_DATA] Updated account data position from extension")
			}
		}
	}

	// Track delivered receipts for connection state update in write transaction
	var deliveredReceipts types.ReceiptDelivery

	if err := ctx.Err(); err != nil {
		return nil, toPos, types.ReceiptDelivery{}, err
	}

	// Process receipts extension (PostProcess)
	if req.Extensions.Receipts != nil && req.Extensions.Receipts.Enabled {
		receiptsResp, receiptsLastPos, receiptsDelivered, err := rp.processReceiptsExtension(ctx, snapshot, connectionKey, userID, req.Extensions.Receipts.Lists, req.Extensions.Receipts.Rooms, fromPos, toPos, responseLists, roomSubscriptions, responseRooms)
		if err != nil {
			if ctx.Err() != nil {
				return nil, toPos, types.ReceiptDelivery{}, ctx.Err()
			}
			logrus.WithError(err).Error("Failed to process receipts extension")
			// Return empty response on error - clients expect the field to be present
			resp.Receipts = &types.ReceiptsResponse{Rooms: make(map[string]synctypes.ClientEvent)}
		} else {
			resp.Receipts = receiptsResp
			deliveredReceipts = receiptsDelivered
			// Update receipt position based on what was actually returned
			logrus.WithFields(logrus.Fields{
				"receiptsLastPos":            receiptsLastPos,
				"updatedPos.ReceiptPosition": updatedPos.ReceiptPosition,
				"toPos.ReceiptPosition":      toPos.ReceiptPosition,
			}).Debug("[RECEIPTS] Checking position update")
			if receiptsLastPos > updatedPos.ReceiptPosition {
				updatedPos.ReceiptPosition = receiptsLastPos
				logrus.WithFields(logrus.Fields{
					"old_pos":      toPos.ReceiptPosition,
					"receipts_pos": receiptsLastPos,
					"new_pos":      receiptsLastPos,
				}).Info("[RECEIPTS] Updated receipt position from extension")
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, toPos, types.ReceiptDelivery{}, err
	}

	// Process typing extension (PostProcess)
	if req.Extensions.Typing != nil && req.Extensions.Typing.Enabled {
		typingResp, err := rp.processTypingExtension(ctx, snapshot, userID, req.Extensions.Typing.Lists, req.Extensions.Typing.Rooms, fromPos, toPos, responseLists, roomSubscriptions, responseRooms)
		if err != nil {
			if ctx.Err() != nil {
				return nil, toPos, types.ReceiptDelivery{}, ctx.Err()
			}
			logrus.WithError(err).Error("Failed to process typing extension")
			// Return empty response on error - clients expect the field to be present
			resp.Typing = &types.TypingResponse{Rooms: make(map[string]synctypes.ClientEvent)}
		} else {
			resp.Typing = typingResp
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, toPos, types.ReceiptDelivery{}, err
	}

	// Process sticky events extension (MSC4354, MSC4480). Its position lives
	// in its own since/next_batch tokens, not in the connection position.
	if req.Extensions.StickyEvents != nil && req.Extensions.StickyEvents.Enabled {
		stickyResp, err := rp.processStickyEventsExtension(ctx, snapshot, userID, req.Extensions.StickyEvents, toPos, responseLists, roomSubscriptions, responseRooms)
		if err != nil {
			if ctx.Err() != nil {
				return nil, toPos, types.ReceiptDelivery{}, ctx.Err()
			}
			logrus.WithError(err).Error("Failed to process sticky events extension")
		} else {
			resp.StickyEvents = stickyResp
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, toPos, types.ReceiptDelivery{}, err
	}

	// Process profile updates extension (MSC4262). A response that could not
	// read every change since the last one serves the profile stream only as
	// far as it read, so the rest follow in the next response.
	if req.Extensions.Profiles != nil && req.Extensions.Profiles.Enabled {
		profilesResp, profilesPos, err := rp.processProfilesExtension(ctx, snapshot, connState, userID, req.Extensions.Profiles, req, fromPos, toPos, responseLists, roomSubscriptions, responseRooms)
		if err != nil {
			if ctx.Err() != nil {
				return nil, toPos, types.ReceiptDelivery{}, ctx.Err()
			}
			logrus.WithError(err).Error("Failed to process profiles extension")
			if fromPos != nil {
				updatedPos.ProfilePosition = fromPos.ProfilePosition
			}
		} else {
			resp.Profiles = profilesResp
			updatedPos.ProfilePosition = profilesPos
		}
	}

	// Process thread subscriptions extension (MSC4308).
	if req.Extensions.ThreadSubscriptions != nil && req.Extensions.ThreadSubscriptions.Enabled {
		threadSubsResp, err := rp.processThreadSubscriptionsExtension(ctx, userID, req.Extensions.ThreadSubscriptions, fromPos, toPos)
		if err != nil {
			if ctx.Err() != nil {
				return nil, toPos, types.ReceiptDelivery{}, ctx.Err()
			}
			logrus.WithError(err).Error("Failed to process thread subscriptions extension")
		} else {
			resp.ThreadSubscriptions = threadSubsResp
		}
	}

	return resp, updatedPos, deliveredReceipts, nil
}

// processThreadSubscriptionsExtension handles the MSC4308 thread subscriptions
// extension, as Synapse does: it returns the latest changes to the user's
// thread subscriptions since the connection position, at most limit of them,
// with a prev_batch token for the /thread_subscriptions endpoint when older
// changes were left out, and nil when nothing changed. An initial sync
// returns the latest changes of all time, unsubscriptions included.
func (rp *RequestPool) processThreadSubscriptionsExtension(
	ctx context.Context,
	userID string,
	req *types.ThreadSubscriptionsRequest,
	fromPos *types.StreamingToken,
	toPos types.StreamingToken,
) (*types.ThreadSubscriptionsResponse, error) {
	limit := types.DefaultThreadSubscriptionsLimit
	if req.Limit != nil {
		limit = *req.Limit
	}
	if limit <= 0 {
		return nil, nil
	}
	limit = min(limit, types.MaxThreadSubscriptionsLimit)

	var from types.StreamPosition
	if fromPos != nil {
		from = fromPos.ThreadSubscriptionPosition
	}
	to := toPos.ThreadSubscriptionPosition
	if to <= from {
		return nil, nil
	}
	updates, err := rp.userAPI.QueryThreadSubscriptionUpdates(ctx, userID, int64(from), int64(to), limit)
	if err != nil || len(updates) == 0 {
		return nil, err
	}
	resp, prevBatch := types.NewThreadSubscriptionsResponse(updates, limit)
	resp.PrevBatch = prevBatch
	return &resp, nil
}

// processToDeviceExtension handles to-device message extension
// Implements stateful tracking with since/next_batch tokens
//
// IMPORTANT: to_device uses its own stateful token (req.Since) separate from
// the main sliding sync position. The client tracks this token independently.
func (rp *RequestPool) processToDeviceExtension(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	userID string,
	deviceID string,
	req *types.ToDeviceRequest,
	fromPos *types.StreamingToken,
	toPos types.StreamingToken,
) (*types.V4ToDeviceResponse, types.StreamPosition, error) {
	// Parse the to_device-specific "since" token from request
	// This is separate from the main sliding sync position
	var from types.StreamPosition
	if req.Since != "" {
		// Parse the since token as a stream position
		sincePos, err := types.NewStreamPositionFromString(req.Since)
		if err != nil {
			// Invalid token - start from 0
			logrus.WithError(err).Warn("Invalid to_device since token, starting from 0")
			from = 0
		} else {
			from = sincePos
		}
	} else {
		// No since token provided - use the main sliding sync position
		// For initial sync, start from 0; for incremental, use fromPos
		if fromPos != nil {
			from = fromPos.SendToDevicePosition
		} else {
			from = 0
		}
	}

	// Get to-device messages from database
	lastPos, events, err := snapshot.SendToDeviceUpdatesForSync(
		ctx, userID, deviceID, from, toPos.SendToDevicePosition,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("SendToDeviceUpdatesForSync failed: %w", err)
	}

	// Apply limit (default 100 as per spec). Synapse accepts a limit of 0,
	// and a negative one must not break the request either; /v4/sync has
	// rejected both already (MSC4538).
	limit := 100
	if req.Limit != nil && *req.Limit > 0 {
		limit = *req.Limit
	}

	// Truncate events if over limit. next_batch then names the last message
	// returned, not the last one read: the client acknowledges everything up
	// to next_batch with its next since, so naming a later position would
	// skip the messages left out, which were never delivered. The connection
	// position stops there too, for clients that send no since.
	deliveredPos := toPos.SendToDevicePosition
	clientEvents := make([]gomatrixserverlib.SendToDeviceEvent, 0, min(len(events), limit))
	for i, event := range events {
		if i >= limit {
			lastPos = events[limit-1].ID
			deliveredPos = lastPos
			break
		}
		clientEvents = append(clientEvents, event.SendToDeviceEvent)
	}

	// Return next_batch token
	// If we hit the limit, the client should use this token to paginate
	// Otherwise, the client has caught up
	return &types.V4ToDeviceResponse{
		NextBatch: fmt.Sprintf("%d", lastPos),
		Events:    clientEvents,
	}, deliveredPos, nil
}

// toDeviceAckPosition returns the position up to which the client has
// acknowledged its to-device messages with this request: the extension's
// since, or without one, the to-device position of the connection position
// it sends. It is 0 when the extension is off or nothing is acknowledged.
func toDeviceAckPosition(req *types.SlidingSyncRequest, since *types.SlidingSyncStreamToken) types.StreamPosition {
	if req.Extensions == nil || req.Extensions.ToDevice == nil || !req.Extensions.ToDevice.Enabled {
		return 0
	}
	if req.Extensions.ToDevice.Since != "" {
		pos, err := types.NewStreamPositionFromString(req.Extensions.ToDevice.Since)
		if err != nil {
			return 0
		}
		return pos
	}
	if since != nil {
		return since.StreamToken.SendToDevicePosition
	}
	return 0
}

// processE2EEExtension handles E2EE device extension (MSC3884)
//
// CRITICAL requirements from research:
// - Initial sync: device_lists must be OMITTED (not nil, omitted entirely)
// - Initial sync: MUST include {"signed_curve25519": 0} for Android compatibility
// - Initial sync: device_unused_fallback_key_types returns empty array []
// - Incremental sync: device_lists includes changed/left users
//
// Reference: /tmp/msc3884_research.md lines 89-93, /tmp/matrix_js_sdk_findings.md lines 86-99.
func (rp *RequestPool) processE2EEExtension(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	userID string,
	deviceID string,
	isInitialSync bool,
	fromPos *types.StreamingToken,
	toPos types.StreamingToken,
) (*types.E2EEResponse, error) { //nolint:unparam // error kept for interface compatibility
	resp := &types.E2EEResponse{
		// CRITICAL: Android compatibility - always include signed_curve25519: 0
		// This will be overwritten if we actually have keys, but ensures the field is present
		DeviceOneTimeKeysCount:             map[string]int{"signed_curve25519": 0},
		DeviceUnusedFallbackKeyTypes:       []string{},
		DeviceUnusedFallbackKeyTypesLegacy: []string{},
		// DeviceLists intentionally nil for initial sync (will be omitted in JSON)
	}

	// Get OTK counts and fallback key types
	var queryRes userapi.QueryOneTimeKeysResponse
	err := rp.userAPI.QueryOneTimeKeys(ctx, &userapi.QueryOneTimeKeysRequest{
		UserID:   userID,
		DeviceID: deviceID,
	}, &queryRes)
	if err != nil || queryRes.Error != nil {
		logrus.WithError(err).Error("QueryOneTimeKeys failed")
		// Continue anyway - return with defaults
	} else {
		// Use the actual key counts
		if queryRes.Count.KeyCount != nil {
			resp.DeviceOneTimeKeysCount = queryRes.Count.KeyCount
			// Ensure signed_curve25519 is always present for Android compatibility
			if _, ok := resp.DeviceOneTimeKeysCount["signed_curve25519"]; !ok {
				resp.DeviceOneTimeKeysCount["signed_curve25519"] = 0
			}
		}
		// Set fallback key types (both new and legacy fields)
		// Ensure we never set nil - use empty slice if no fallback keys
		if queryRes.UnusedFallbackAlgorithms != nil {
			resp.DeviceUnusedFallbackKeyTypes = queryRes.UnusedFallbackAlgorithms
			resp.DeviceUnusedFallbackKeyTypesLegacy = queryRes.UnusedFallbackAlgorithms
		}
		// If nil, keep the empty slice we initialized above
	}

	// For incremental sync, get device list changes
	// For initial sync, device_lists MUST be omitted (left as nil)
	if !isInitialSync && fromPos != nil {
		// Only call DeviceListCatchup if the position has actually changed
		// If positions are the same, there are no device list changes
		if fromPos.DeviceListPosition != toPos.DeviceListPosition {
			// Create a minimal v3 Response to use with DeviceListCatchup
			tempResponse := &types.Response{
				DeviceLists: &types.DeviceLists{
					Changed: []string{},
					Left:    []string{},
				},
				// Need to populate Rooms.Join for DeviceListCatchup to detect newly joined rooms
				Rooms: &types.RoomsResponse{
					Join:   make(map[string]*types.JoinResponse),
					Invite: make(map[string]*types.InviteResponse),
					Leave:  make(map[string]*types.LeaveResponse),
				},
			}

			// Call DeviceListCatchup to get device list changes
			_, _, err := internal.DeviceListCatchup(
				ctx, snapshot, rp.userAPI, rp.rsAPI,
				userID, tempResponse,
				fromPos.DeviceListPosition, toPos.DeviceListPosition,
			)
			if err != nil {
				logrus.WithError(err).Error("DeviceListCatchup failed")
				// Continue anyway - device lists are optional
				// IMPORTANT: Still set DeviceLists even on error (Synapse always includes it)
				resp.DeviceLists = &types.DeviceLists{
					Changed: []string{},
					Left:    []string{},
				}
			} else {
				// CRITICAL: Always set DeviceLists for incremental sync, even if empty
				// Synapse always includes device_lists field with empty arrays when no changes
				// Clients expect this field to be present to distinguish "no changes" from "not tracking"
				resp.DeviceLists = tempResponse.DeviceLists
			}
		}
	}

	// Always return e2ee extension if requested
	// Synapse behavior: always returns OTK counts and device lists (even if empty arrays)
	return resp, nil
}

// accountDataStream is the SlidingSyncConnectionStream "stream" tag for
// per-room account_data tracking, independent of the "events" stream used
// for the timeline.
const accountDataStream = "account_data"

// globalAccountDataScope is the room ID under which the account_data stream
// state tracks global account data, which has no room. The connection has
// never sent global account data while it has no state there, so enabling
// the extension partway through a connection sends all of it (MSC4548).
const globalAccountDataScope = ""

// accountDataQueryLimit caps a single account data range query. A result that
// hits it is truncated to the OLDEST rows, since the query orders by id ASC,
// so the caller must not advance any room past the position it did return.
const accountDataQueryLimit = 1000

func warnIfAccountDataTruncated(dataTypes map[string][]string, limit int, from types.StreamPosition) {
	rows := 0
	for _, types := range dataTypes {
		rows += len(types)
	}
	if rows < limit {
		return
	}
	logrus.WithFields(logrus.Fields{
		"rows":  rows,
		"limit": limit,
		"from":  from,
	}).Warn("[ACCOUNT_DATA] Query hit the row limit; the newest changes are deferred to the next response")
}

// accountDataFloorForRoom returns the lower bound for a room's account data
// query: connectionFrom, or the room's own account_data LastToken if that's
// older (the room fell behind while excluded from prior responses). Returns
// 0 if this connection has never sent the room's account data before.
func accountDataFloorForRoom(connState *V4ConnectionState, roomID string, connectionFrom types.StreamPosition) types.StreamPosition {
	if connState == nil || connState.PreviousStreamStates == nil {
		return 0
	}
	prevState, ok := connState.PreviousStreamStates[roomID][accountDataStream]
	if !ok || prevState.LastToken == "" {
		return 0
	}
	tok, err := types.NewStreamTokenFromString(prevState.LastToken)
	if err != nil {
		return 0
	}
	if tok.AccountDataPosition < connectionFrom {
		return tok.AccountDataPosition
	}
	return connectionFrom
}

// persistAccountDataRoomState records, per room, the account data position that
// was actually delivered in this response. The roomPositions map must give the
// position each room was served up to, which is not necessarily toPos: the
// query is limited, so a truncated result must not advance a room past what it
// returned.
//
// Rooms absent from roomPositions are carried over by
// copyForwardAccountDataState, which runs for every response rather than only
// this one, so a request that does not reach here still keeps their floors.
//
// No-ops on interrupt/timeout/disconnect responses (ConnectionPosition == 0),
// which return the unchanged pos and create no position to write to.
func (rp *RequestPool) persistAccountDataRoomState(
	ctx context.Context,
	connState *V4ConnectionState,
	roomPositions map[string]types.StreamPosition,
	toPos types.StreamingToken,
) {
	if connState == nil || connState.ConnectionPosition == 0 {
		return
	}
	if connState.AccountDataWritten == nil {
		connState.AccountDataWritten = make(map[string]bool, len(roomPositions))
	}

	for roomID, pos := range roomPositions {
		if ctx.Err() != nil {
			return
		}
		delivered := toPos
		delivered.AccountDataPosition = pos
		if err := rp.db.UpdateConnectionStream(ctx, connState.ConnectionPosition, roomID, accountDataStream, "live", delivered.String()); err != nil {
			if rp.stopAfterWriteError(ctx, connState) {
				return
			}
			logrus.WithError(err).WithField("room_id", roomID).Warn("[ACCOUNT_DATA] Failed to persist account_data stream state")
			continue
		}
		connState.AccountDataWritten[roomID] = true
	}
}

// copyForwardAccountDataState carries every room's stored account_data position
// onto this connection position, for rooms persistAccountDataRoomState did not
// already write.
//
// It must run on every response, not just those that processed the account_data
// extension. Old positions are cascade-deleted on the next request, so a single
// response that skipped the extension - because the client did not enable it,
// or because the query errored - would otherwise leave the new position with no
// account_data rows at all, resetting every room's floor to 0 and forcing a
// full rescan and re-delivery.
//
// Once the request context is done it stops and returns the context's error:
// the response will not be acknowledged, so the position it writes to is
// discarded on the client's retry anyway.
func (rp *RequestPool) copyForwardAccountDataState(ctx context.Context, connState *V4ConnectionState) error {
	if connState == nil || connState.ConnectionPosition == 0 {
		return nil
	}

	for roomID, streams := range connState.PreviousStreamStates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if connState.AccountDataWritten[roomID] {
			continue // already written with a fresh position
		}
		prevState, ok := streams[accountDataStream]
		if !ok {
			continue
		}
		if err := rp.db.UpdateConnectionStream(ctx, connState.ConnectionPosition, roomID, accountDataStream, prevState.RoomStatus, prevState.LastToken); err != nil {
			if rp.stopAfterWriteError(ctx, connState) {
				return ctx.Err()
			}
			logrus.WithError(err).WithField("room_id", roomID).Warn("[ACCOUNT_DATA] Failed to copy forward account_data stream state")
		}
	}
	return nil
}

// processAccountDataExtension handles account data extension.
func (rp *RequestPool) processAccountDataExtension(
	ctx context.Context,
	snapshot storage.DatabaseTransaction,
	connState *V4ConnectionState,
	userID string,
	requestedLists []string, // Optional list filter from request (MSC3959)
	requestedRooms []string, // Optional room filter from request (MSC3960)
	fromPos *types.StreamingToken,
	toPos types.StreamingToken,
	actualLists map[string]types.SlidingList, // Actual lists in response
	actualRoomSubscriptions map[string]bool, // Actual room subscriptions from request
) (*types.AccountDataResponse, types.StreamPosition, error) {
	// Get the "from" position for incremental sync
	var from types.StreamPosition
	if fromPos != nil {
		from = fromPos.AccountDataPosition
	}

	// Determine which rooms to process using unified helper (MSC3959/MSC3960)
	relevantRoomIDs := findRelevantRoomIDsForExtension(
		requestedLists,
		requestedRooms,
		actualLists,
		actualRoomSubscriptions,
	)

	logrus.WithFields(logrus.Fields{
		"requested_lists": requestedLists,
		"requested_rooms": requestedRooms,
		"relevant_rooms":  len(relevantRoomIDs),
	}).Debug("[ACCOUNT_DATA] Filtered rooms for extension")

	// Gap recovery is only attempted when this response can record the result.
	// An interrupt response (ConnectionPosition == 0) returns the unchanged pos
	// and writes no state, so widening there would redo the same scan and
	// re-send the same payload on every long-poll timeout, forever.
	canPersist := connState != nil && connState.ConnectionPosition != 0

	// Group the rooms that need a wider floor than the connection's own by the
	// floor they need, so that one extra query serves all rooms sharing it
	// (rooms new to this connection all share floor 0). Rooms that kept up are
	// served by the base query below, which is also the only query global
	// account data is taken from: widening a single shared query to the oldest
	// floor would re-deliver every relevant room's full history along with it.
	//
	// Global account data is tracked the same way, under
	// globalAccountDataScope, so that it is all sent the first time the
	// extension is enabled on a connection, not only what changed since pos.
	gapFloors := make(map[types.StreamPosition][]string)
	if canPersist {
		for roomID := range relevantRoomIDs {
			floor := accountDataFloorForRoom(connState, roomID, from)
			if floor >= from {
				continue
			}
			gapFloors[floor] = append(gapFloors[floor], roomID)
		}
		if floor := accountDataFloorForRoom(connState, globalAccountDataScope, from); floor < from {
			gapFloors[floor] = append(gapFloors[floor], globalAccountDataScope)
		}
	}

	// Create range for account data query
	r := types.Range{
		From: from,
		To:   toPos.AccountDataPosition,
	}

	logrus.WithFields(logrus.Fields{
		"user_id":        userID,
		"from":           from,
		"to":             toPos.AccountDataPosition,
		"is_initial":     fromPos == nil,
		"relevant_rooms": len(relevantRoomIDs),
		"gap_floors":     len(gapFloors),
	}).Debug("[ACCOUNT_DATA] Querying account data range")

	// Get account data changes in this range
	// Use filter with high limit to get all account data (typically < 100 events)
	filter := synctypes.EventFilter{
		Limit: accountDataQueryLimit,
	}
	dataTypes, lastPos, err := snapshot.GetAccountDataInRange(ctx, userID, r, &filter)
	if err != nil {
		return nil, 0, fmt.Errorf("GetAccountDataInRange failed: %w", err)
	}
	warnIfAccountDataTruncated(dataTypes, accountDataQueryLimit, from)

	// Position each relevant room was actually served up to. The base query
	// covers the rooms that kept up; gap rooms start at their own floor, which
	// is where they stay if their query fails, so a failure re-tries the gap
	// next time instead of skipping it.
	roomPositions := make(map[string]types.StreamPosition, len(relevantRoomIDs)+1)
	for roomID := range relevantRoomIDs {
		roomPositions[roomID] = lastPos
	}
	roomPositions[globalAccountDataScope] = lastPos
	// globalFrom is where global account data was read from, which is below
	// from when the connection has not sent it before.
	globalFrom := from
	// changedGlobal are the global types that changed after from, which the
	// base query read. Those deleted since are sent with empty content even
	// when all global account data is sent from globalFrom 0, as for a
	// connection whose positions predate tracking global account data.
	changedGlobal := make(map[string]bool, len(dataTypes[globalAccountDataScope]))
	for _, dataType := range dataTypes[globalAccountDataScope] {
		changedGlobal[dataType] = true
	}
	for floor, rooms := range gapFloors {
		for _, roomID := range rooms {
			roomPositions[roomID] = floor
		}
		gapTypes, gapLastPos, gapErr := snapshot.GetAccountDataInRange(ctx, userID, types.Range{
			From: floor,
			To:   toPos.AccountDataPosition,
		}, &filter)
		if gapErr != nil {
			logrus.WithError(gapErr).WithFields(logrus.Fields{
				"floor": floor,
				"rooms": len(rooms),
			}).Warn("[ACCOUNT_DATA] Gap query failed; leaving those rooms to retry next response")
			continue
		}
		warnIfAccountDataTruncated(gapTypes, accountDataQueryLimit, floor)
		for _, roomID := range rooms {
			if types, ok := gapTypes[roomID]; ok {
				dataTypes[roomID] = types
			}
			roomPositions[roomID] = gapLastPos
			if roomID == globalAccountDataScope {
				globalFrom = floor
			}
		}
	}

	logrus.WithFields(logrus.Fields{
		"num_rooms":  len(dataTypes),
		"data_types": dataTypes,
	}).Debug("[ACCOUNT_DATA] Got account data types")

	// Create response
	resp := &types.AccountDataResponse{
		Global: []synctypes.ClientEvent{},
		Rooms:  make(map[string][]synctypes.ClientEvent),
	}

	// Iterate over rooms and data types
	for roomID, dataTypeList := range dataTypes {
		// Skip rooms not in the relevant set (unless it's global account data)
		if roomID != "" && !relevantRoomIDs[roomID] {
			continue
		}

		// Query each data type from userAPI
		for _, dataType := range dataTypeList {
			dataReq := userapi.QueryAccountDataRequest{
				UserID:   userID,
				RoomID:   roomID,
				DataType: dataType,
			}
			dataRes := userapi.QueryAccountDataResponse{}
			err = rp.userAPI.QueryAccountData(ctx, &dataReq, &dataRes)
			if err != nil {
				logrus.WithError(err).Error("QueryAccountData failed")
				continue
			}

			// Separate global vs per-room account data
			if roomID == "" {
				// Global account data. An entry that changed in range but no
				// longer exists was deleted; incremental responses carry it
				// with empty content, as Synapse does (MSC3391). Responses
				// that send all global account data leave it out instead.
				globalData, ok := dataRes.GlobalAccountData[dataType]
				if !ok && (globalFrom > 0 || changedGlobal[dataType]) && from > 0 {
					globalData, ok = []byte("{}"), true
				}
				if ok {
					resp.Global = append(resp.Global, synctypes.ClientEvent{
						Type:    dataType,
						Content: spec.RawJSON(globalData),
					})
				}
			} else {
				// Per-room account data
				if roomData, ok := dataRes.RoomAccountData[roomID][dataType]; ok {
					if resp.Rooms[roomID] == nil {
						resp.Rooms[roomID] = []synctypes.ClientEvent{}
					}
					resp.Rooms[roomID] = append(resp.Rooms[roomID], synctypes.ClientEvent{
						Type:    dataType,
						Content: spec.RawJSON(roomData),
					})
				}
			}
		}
	}

	// Always return account_data extension if requested
	// Synapse may return empty account_data objects
	logrus.WithFields(logrus.Fields{
		"num_global":    len(resp.Global),
		"num_room_keys": len(resp.Rooms),
	}).Debug("[ACCOUNT_DATA] Returning account data response")

	rp.persistAccountDataRoomState(ctx, connState, roomPositions, toPos)

	// Return lastPos so v4 sync can update the account data position in the response token
	return resp, lastPos, nil
}

// processReceiptsExtension handles read receipts extension (MSC4551).
// IMPORTANT: Response contains a SINGLE event per room, not an array (matrix-js-sdk expects this).
//
// The connection records which rooms have been in scope and which receipts
// it was given, tagged with the position of the response that gave them, so
// a retried request gets the receipts of a lost response again: v4.go forgets
// what the responses after the request's pos delivered. For each joined room
// in scope:
//   - a room in scope for the first time gets its initial receipts: the
//     user's own receipts, and every user's receipts on the events in its
//     timeline in this response. The others count as delivered, so later
//     responses only carry their changes;
//   - any other room gets the receipts that changed since they were last
//     delivered, which covers the changes made while it was out of scope;
//   - a room sent with initial or expanded_timeline also gets every user's
//     receipts on the events in its timeline, and the user's own receipts,
//     delivered or not.
//
// A room the user is not joined to gets nothing, and the connection forgets
// its receipts, so that it is in scope for the first time again when the
// user rejoins.
func (rp *RequestPool) processReceiptsExtension(
	ctx context.Context,
	_ storage.DatabaseTransaction, // unused - using fresh snapshot for read committed isolation
	connectionKey int64, // For per-connection receipt tracking
	userID string,
	requestedLists []string, // Optional list filter from request (MSC3959)
	requestedRooms []string, // Optional room filter from request (MSC3960)
	_ *types.StreamingToken, // unused - using per-connection event-ID tracking
	_ types.StreamingToken, // unused - using per-connection event-ID tracking
	actualLists map[string]types.SlidingList, // Actual lists in response
	actualRoomSubscriptions map[string]bool, // Actual room subscriptions from request
	responseRooms map[string]types.SlidingRoomData, // Rooms in the response, whose timelines bring receipts
) (*types.ReceiptsResponse, types.StreamPosition, types.ReceiptDelivery, error) {
	var delivery types.ReceiptDelivery
	// Determine which rooms to process using unified helper (MSC3959/MSC3960)
	relevantRoomIDs := findRelevantRoomIDsForExtension(
		requestedLists,
		requestedRooms,
		actualLists,
		actualRoomSubscriptions,
	)

	// Query receipts using a fresh transaction instead of the snapshot: the
	// snapshot uses REPEATABLE READ isolation and may not see receipts
	// committed since it began, which left unread badges stuck on
	// long-polling connections.
	freshSnapshot, err := rp.db.NewDatabaseSnapshot(ctx)
	if err != nil {
		return nil, 0, delivery, fmt.Errorf("failed to create fresh snapshot for receipts: %w", err)
	}
	defer func() { _ = freshSnapshot.Rollback() }()

	relevant := make([]string, 0, len(relevantRoomIDs))
	for roomID := range relevantRoomIDs {
		relevant = append(relevant, roomID)
	}
	state, err := freshSnapshot.SelectConnectionReceiptState(ctx, connectionKey, relevant)
	if err != nil {
		return nil, 0, delivery, fmt.Errorf("SelectConnectionReceiptState failed: %w", err)
	}

	// Lists keep invites and rooms the user was kicked from, whose receipts
	// the user must not see. Memberships come from the same fresh snapshot
	// as the receipts, so that a kick is not missed by an older snapshot.
	// The rooms the connection holds receipts of are checked too, so that a
	// room the user left is forgotten even once it is out of every list.
	candidates := make(map[string]bool, len(relevantRoomIDs)+len(state.InScope))
	for roomID := range relevantRoomIDs {
		candidates[roomID] = true
	}
	for roomID := range state.InScope {
		candidates[roomID] = true
	}
	joinedRooms, err := joinedRoomsOnly(ctx, freshSnapshot, userID, candidates)
	if err != nil {
		return nil, 0, delivery, err
	}
	joined := make(map[string]bool, len(joinedRooms))
	for _, roomID := range joinedRooms {
		joined[roomID] = true
	}
	// Only changes are recorded: the rooms entering scope, and the rooms
	// the user left that the connection still holds receipts of.
	for roomID := range relevantRoomIDs {
		if joined[roomID] && !state.InScope[roomID] {
			delivery.InScope = append(delivery.InScope, roomID)
		}
	}
	for roomID := range state.InScope {
		if !joined[roomID] {
			delivery.ForgetRooms = append(delivery.ForgetRooms, roomID)
		}
	}

	windows := receiptTimelineWindows(responseRooms)
	receiptsByRoom := make(map[string][]types.OutputReceiptEvent)
	withheld := 0
	for _, receipt := range state.Latest {
		if !joined[receipt.RoomID] {
			continue
		}
		// Don't send private read receipts to other users
		if receipt.Type == "m.read.private" && userID != receipt.UserID {
			continue
		}
		window, reset := windows[receipt.RoomID]
		_, inTimeline := window[receipt.EventID]
		own := receipt.UserID == userID
		deliveredEventID, delivered := state.Delivered[receipt.Key()]
		changed := !delivered || deliveredEventID != receipt.EventID
		var send bool
		if state.InScope[receipt.RoomID] {
			// The user's own receipts are sent again with a reset room as
			// well: they drive its unread state.
			send = changed || inTimeline || (reset && own)
		} else {
			send = own || inTimeline
		}
		if !send {
			if changed {
				// Part of the room's receipts the client is not sent when
				// the room enters scope; later responses carry its changes.
				delivery.Receipts = append(delivery.Receipts, receipt)
				withheld++
			}
			continue
		}
		receiptsByRoom[receipt.RoomID] = append(receiptsByRoom[receipt.RoomID], receipt)
	}

	// Create response with single event per room
	resp := &types.ReceiptsResponse{
		Rooms: make(map[string]synctypes.ClientEvent),
	}

	for roomID, roomReceipts := range receiptsByRoom {
		ev := synctypes.ClientEvent{
			Type: "m.receipt",
		}
		// Structure: eventID -> receiptType -> userID -> {ts}
		// This allows m.read and m.read.private to be serialized separately
		content := make(map[string]map[string]map[string]ReceiptTS)
		for _, receipt := range roomReceipts {
			eventContent, ok := content[receipt.EventID]
			if !ok {
				eventContent = make(map[string]map[string]ReceiptTS)
				content[receipt.EventID] = eventContent
			}
			typeContent, ok := eventContent[receipt.Type]
			if !ok {
				typeContent = make(map[string]ReceiptTS)
				eventContent[receipt.Type] = typeContent
			}

			// Sliding sync has one receipt event per room. If two threads share
			// a wire key, deliver the second on the next response rather than
			// overwriting it and falsely recording it as delivered.
			if _, exists := typeContent[receipt.UserID]; exists {
				continue
			}
			typeContent[receipt.UserID] = ReceiptTS{TS: receipt.Timestamp, ThreadID: receipt.ThreadID}

			// Collect this receipt for connection state update (will be done in write transaction later)
			delivery.Receipts = append(delivery.Receipts, receipt)
		}
		ev.Content, err = json.Marshal(content)
		if err != nil {
			logrus.WithError(err).Error("Failed to marshal receipt content")
			continue
		}

		resp.Rooms[roomID] = ev
	}

	logrus.WithFields(logrus.Fields{
		"connection_key":  connectionKey,
		"num_rooms":       len(resp.Rooms),
		"delivered_count": len(delivery.Receipts) - withheld,
		"withheld_count":  withheld,
		"user_id":         userID,
	}).Debug("[RECEIPTS] Returning receipts response")

	// Receipts are tracked per connection and position in their own table,
	// not by the stream position in the token.
	return resp, 0, delivery, nil
}

// receiptTimelineWindows returns, for each response room sent with initial
// or expanded_timeline, the IDs of the events in its returned timeline, whose
// receipts the response carries (MSC4551).
func receiptTimelineWindows(responseRooms map[string]types.SlidingRoomData) map[string]map[string]struct{} {
	windows := make(map[string]map[string]struct{})
	for roomID, room := range responseRooms {
		if !room.Initial && !room.ExpandedTimeline {
			continue
		}
		window := make(map[string]struct{}, len(room.Timeline))
		for _, ev := range room.Timeline {
			window[ev.EventID] = struct{}{}
		}
		windows[roomID] = window
	}
	return windows
}

// ReceiptMRead represents the m.read structure for receipts.
type ReceiptMRead struct {
	User map[string]ReceiptTS `json:"m.read"`
}

// ReceiptTS represents a receipt timestamp.
type ReceiptTS struct {
	ThreadID string         `json:"thread_id,omitempty"`
	TS       spec.Timestamp `json:"ts"`
}

// processTypingExtension handles typing notifications extension
// IMPORTANT: Response contains a SINGLE event per room, not an array (matrix-js-sdk expects this).
func (rp *RequestPool) processTypingExtension(
	ctx context.Context,
	snapshot storage.DatabaseTransaction, // for memberships only; typing comes from the EDUCache
	userID string,
	requestedLists []string, // Optional list filter from request (MSC3959)
	requestedRooms []string, // Optional room filter from request (MSC3960)
	fromPos *types.StreamingToken,
	_ types.StreamingToken, // unused - typing uses fromPos only
	actualLists map[string]types.SlidingList, // Actual lists in response
	actualRoomSubscriptions map[string]bool, // Actual room subscriptions from request
	responseRooms map[string]types.SlidingRoomData, // Rooms in the response, which get their current typing state
) (*types.TypingResponse, error) {
	// Determine which rooms to process using unified helper (MSC3959/MSC3960)
	relevantRoomIDs := findRelevantRoomIDsForExtension(
		requestedLists,
		requestedRooms,
		actualLists,
		actualRoomSubscriptions,
	)

	// Lists keep invites and rooms the user was kicked from, in which the
	// user must not see who is typing.
	roomsToCheck, err := joinedRoomsOnly(ctx, snapshot, userID, relevantRoomIDs)
	if err != nil {
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"requested_lists": requestedLists,
		"requested_rooms": requestedRooms,
		"relevant_rooms":  len(roomsToCheck),
	}).Debug("[TYPING] Filtered rooms for extension")

	// Get the "from" position for incremental sync
	var from types.StreamPosition
	if fromPos != nil {
		from = fromPos.TypingPosition
	}

	// Access the typing stream provider's EDUCache
	// Cast to TypingStreamProvider to access the EDUCache
	typingProvider, ok := rp.streams.TypingStreamProvider.(*streams.TypingStreamProvider)
	if !ok {
		return nil, fmt.Errorf("failed to cast TypingStreamProvider")
	}

	// Create response
	resp := &types.TypingResponse{
		Rooms: make(map[string]synctypes.ClientEvent),
	}

	// Check each room for typing updates. A room the response sends may have
	// just entered scope, as rooms entering a list's range do, and must then
	// carry its current typing state even if it did not change since pos
	// (MSC4508). Typing is latest state, so sending it again for a room that
	// was in scope already does no harm.
	for _, roomID := range roomsToCheck {
		users, updated := typingProvider.EDUCache.GetTypingUsersIfUpdatedAfter(roomID, int64(from))
		if _, sent := responseRooms[roomID]; sent && !updated {
			// Any typing the cache knows of for the room, including that
			// nobody types any more, which a client that saw someone typing
			// before the room left scope needs.
			users, updated = typingProvider.EDUCache.GetTypingUsersIfUpdatedAfter(roomID, 0)
		}
		if !updated {
			continue // No typing updates for this room
		}

		// Create typing event for this room
		ev := synctypes.ClientEvent{
			Type: "m.typing",
		}

		// Marshal typing user IDs into content
		var err error
		ev.Content, err = json.Marshal(map[string]any{
			"user_ids": users,
		})
		if err != nil {
			logrus.WithError(err).Error("Failed to marshal typing content")
			continue
		}

		resp.Rooms[roomID] = ev
	}

	// Always return typing extension if requested
	// Synapse may return empty typing objects
	return resp, nil
}

// joinedRoomsOnly returns the rooms of roomIDs the user is joined to. Typing
// and receipts, like the ephemeral events of /v3/sync, are only for joined
// rooms, while sliding sync lists also hold invites, knocks and rooms the
// user was kicked from.
func joinedRoomsOnly(ctx context.Context, snapshot storage.DatabaseTransaction, userID string, roomIDs map[string]bool) ([]string, error) {
	if len(roomIDs) == 0 {
		return nil, nil
	}
	joined, err := snapshot.RoomIDsWithMembership(ctx, userID, spec.Join)
	if err != nil {
		return nil, fmt.Errorf("RoomIDsWithMembership failed: %w", err)
	}
	rooms := make([]string, 0, min(len(joined), len(roomIDs)))
	for _, roomID := range joined {
		if roomIDs[roomID] {
			rooms = append(rooms, roomID)
		}
	}
	return rooms, nil
}
