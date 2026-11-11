// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package api

import "errors"

// ErrPartialStatePrevEvents is returned by ResyncPartialStateEvent when a
// prev event of the event still has partial state (MSC3902). The resync then
// takes a new batch, which holds that prev event first.
var ErrPartialStatePrevEvents = errors.New("prev events still have partial state")
