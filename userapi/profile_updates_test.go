// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package userapi_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/internal/eventutil"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/userapi/api"
	"codefloe.com/pat-s/zendrite/userapi/internal"
)

// profileUpdateRecorder records the profile updates published for the sync
// API.
type profileUpdateRecorder struct {
	mu      sync.Mutex
	updates []string
}

func (r *profileUpdateRecorder) PublishMsg(msg *nats.Msg, _ ...nats.PubOpt) (*nats.PubAck, error) {
	if msg.Subject == "profile_updates" {
		var update eventutil.ProfileUpdate
		if err := json.Unmarshal(msg.Data, &update); err != nil {
			return nil, err
		}
		r.mu.Lock()
		for _, field := range update.Fields {
			r.updates = append(r.updates, msg.Header.Get(jetstream.UserID)+" "+field)
		}
		r.mu.Unlock()
	}
	return &nats.PubAck{}, nil
}

func (r *profileUpdateRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	updates := r.updates
	r.updates = nil
	return updates
}

// Every change to a profile is published for the sync API's profile updates
// stream (MSC4262), and setting the display name or avatar URL to its
// current value is not.
func TestProfileChangesArePublished(t *testing.T) {
	test.WithAllDatabases(t, func(t *testing.T, dbType test.DBType) {
		ctx := context.Background()
		recorder := &profileUpdateRecorder{}
		userAPI, accountDB, closeDB := MustMakeInternalAPI(t, apiTestOpts{}, dbType, recorder)
		defer closeDB()
		intAPI, ok := userAPI.(*internal.UserInternalAPI)
		require.True(t, ok)
		intAPI.SyncProducer.ProfileUpdateTopic = "profile_updates"
		_, err := accountDB.CreateAccount(ctx, "alice", serverName, "foobar", "", api.AccountTypeUser)
		require.NoError(t, err)
		alice := "@alice:" + string(serverName)

		_, _, err = userAPI.SetDisplayName(ctx, "alice", serverName, "Alice")
		require.NoError(t, err)
		_, _, err = userAPI.SetDisplayName(ctx, "alice", serverName, "Alice")
		require.NoError(t, err)
		_, _, err = userAPI.SetAvatarURL(ctx, "alice", serverName, "mxc://example.com/alice")
		require.NoError(t, err)
		require.NoError(t, userAPI.SetProfileField(ctx, "alice", serverName, "m.tz", json.RawMessage(`"Europe/Berlin"`)))
		require.NoError(t, userAPI.DeleteProfileField(ctx, "alice", serverName, "m.tz"))
		require.Equal(t, []string{
			alice + " displayname",
			alice + " avatar_url",
			alice + " m.tz",
			alice + " m.tz",
		}, recorder.take())
	})
}
