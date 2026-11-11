// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package internal

import (
	"context"

	"codefloe.com/pat-s/gomatrixserverlib/spec"
	"github.com/matrix-org/util"
)

// removeDevicePushers deletes the pushers that the deleted devices
// registered, as Synapse does when devices are deleted or log out. With
// MSC3881 every pusher knows its device, so a pusher another device disabled
// or enabled still goes with the device that registered it. The devices are
// already gone, so a failure is logged rather than stopping the deletion of
// their keys.
func (a *UserInternalAPI) removeDevicePushers(ctx context.Context, localpart string, serverName spec.ServerName, deviceIDs []string) {
	if err := a.DB.RemovePushersByDevices(ctx, localpart, serverName, deviceIDs); err != nil {
		util.GetLogger(ctx).WithError(err).WithField("localpart", localpart).Error("Failed to remove the pushers of deleted devices")
	}
}
