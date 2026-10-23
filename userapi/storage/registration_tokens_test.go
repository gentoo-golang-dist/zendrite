// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	clientapi "codefloe.com/pat-s/zendrite/clientapi/api"
	"codefloe.com/pat-s/zendrite/test"
)

// Updates must change the stored token. On SQLite the update statements once
// bound their arguments in the wrong order, so they matched no row and the
// admin API reported success while nothing changed.
func Test_UpdateRegistrationToken(t *testing.T) {
	ptr := func(v int64) *int64 { return &v }

	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		db, closeDB := mustCreateUserDatabase(t, dbType)
		defer closeDB()
		ctx := context.Background()

		token := "token1"
		usesAllowed := int32(5)
		expiryTime := time.Now().Add(time.Hour).UnixMilli()
		zero := int32(0)
		created, err := db.InsertRegistrationToken(ctx, &clientapi.RegistrationToken{
			Token:       &token,
			UsesAllowed: &usesAllowed,
			Pending:     &zero,
			Completed:   &zero,
			ExpiryTime:  &expiryTime,
		})
		require.NoError(t, err)
		require.True(t, created)

		newExpiry := time.Now().Add(48 * time.Hour).UnixMilli()
		got, err := db.UpdateRegistrationToken(ctx, token, map[string]any{"usesAllowed": ptr(10)})
		require.NoError(t, err)
		require.EqualValues(t, 10, *got.UsesAllowed)
		require.Equal(t, expiryTime, *got.ExpiryTime)

		got, err = db.UpdateRegistrationToken(ctx, token, map[string]any{"expiryTime": &newExpiry})
		require.NoError(t, err)
		require.EqualValues(t, 10, *got.UsesAllowed)
		require.Equal(t, newExpiry, *got.ExpiryTime)

		got, err = db.UpdateRegistrationToken(ctx, token, map[string]any{"usesAllowed": ptr(20), "expiryTime": (*int64)(nil)})
		require.NoError(t, err)
		require.EqualValues(t, 20, *got.UsesAllowed)
		require.Nil(t, got.ExpiryTime)
	})
}
