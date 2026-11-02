// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"

	"codefloe.com/pat-s/gomatrixserverlib/spec"

	"codefloe.com/pat-s/zendrite/userapi/api"
)

func (a *UserInternalAPI) PerformRendezvousCreate(ctx context.Context, session *api.RendezvousSession, maxAnonymous, maxPerUser int) error {
	return a.DB.CreateRendezvousSession(ctx, session, maxAnonymous, maxPerUser)
}

func (a *UserInternalAPI) QueryRendezvousSession(ctx context.Context, id string, kind api.RendezvousKind, now spec.Timestamp) (*api.RendezvousSession, error) {
	return a.DB.GetRendezvousSession(ctx, id, kind, now)
}

func (a *UserInternalAPI) QueryRendezvousTxn(ctx context.Context, id string, kind api.RendezvousKind, txnID string, now spec.Timestamp) (int64, bool, error) {
	return a.DB.GetRendezvousTxn(ctx, id, kind, txnID, now)
}

func (a *UserInternalAPI) PerformRendezvousWrite(ctx context.Context, write *api.RendezvousWrite) (*api.RendezvousSession, error) {
	return a.DB.WriteRendezvousSession(ctx, write)
}

func (a *UserInternalAPI) PerformRendezvousDelete(ctx context.Context, id string, kind api.RendezvousKind, now spec.Timestamp) (bool, error) {
	return a.DB.DeleteRendezvousSession(ctx, id, kind, now)
}
