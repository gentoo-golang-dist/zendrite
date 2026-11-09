// Copyright 2024 New Vector Ltd.
// Copyright 2020 The Matrix.org Foundation C.I.C.
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//go:build wasm

package sqlutil

import (
	"errors"

	"modernc.org/sqlite"
	lib "modernc.org/sqlite/lib"
)

// IsUniqueConstraintViolationErr returns true if the error is an unique_violation error.
// It also recognizes a wrapped error.
func IsUniqueConstraintViolationErr(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		// Code is the extended result code, e.g. SQLITE_CONSTRAINT_UNIQUE;
		// its low byte is the primary one.
		return sqliteErr.Code()&0xff == lib.SQLITE_CONSTRAINT
	}
	return false
}
