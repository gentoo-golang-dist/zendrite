// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package sync

import (
	"net/http"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"

	"codefloe.com/pat-s/zendrite/syncapi/types"
)

// validateStableExtensions applies the request checks of MSC4538 to a
// /v4/sync request: a to_device limit must be positive, and its since must
// be a next_batch the extension could have issued.
func validateStableExtensions(ext *types.ExtensionRequest) *util.JSONResponse {
	if ext == nil || ext.ToDevice == nil || !ext.ToDevice.Enabled {
		return nil
	}
	if ext.ToDevice.Limit != nil && *ext.ToDevice.Limit <= 0 {
		return &util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: spec.BadJSON("to_device limit must be a positive integer"),
		}
	}
	if ext.ToDevice.Since != "" {
		if pos, err := types.NewStreamPositionFromString(ext.ToDevice.Since); err != nil || pos < 0 {
			return &util.JSONResponse{
				Code: http.StatusBadRequest,
				JSON: spec.InvalidParam("unknown to_device since"),
			}
		}
	}
	return nil
}
