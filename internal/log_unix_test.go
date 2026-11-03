// Copyright 2026 The Zendrite Authors
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

//go:build !windows

package internal

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
)

// A std hook configured at a level other than info must not be joined by the
// default std hook at info, or every message at both levels is written to
// stdout twice.
func TestSetupHookLoggingWritesEachMessageOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		hooks []config.LogrusHook
		want  map[logrus.Level]int
	}{
		"no std hook": {
			want: map[logrus.Level]int{logrus.WarnLevel: 1, logrus.InfoLevel: 1, logrus.DebugLevel: 0},
		},
		"std at warn": {
			hooks: []config.LogrusHook{{Type: "std", Level: "warn"}},
			want:  map[logrus.Level]int{logrus.WarnLevel: 1, logrus.InfoLevel: 0},
		},
		"std at debug": {
			hooks: []config.LogrusHook{{Type: "std", Level: "debug"}},
			want:  map[logrus.Level]int{logrus.WarnLevel: 1, logrus.InfoLevel: 1, logrus.DebugLevel: 1},
		},
		"std at info and debug": {
			hooks: []config.LogrusHook{{Type: "std", Level: "info"}, {Type: "std", Level: "debug"}},
			want:  map[logrus.Level]int{logrus.WarnLevel: 1, logrus.InfoLevel: 1, logrus.DebugLevel: 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			logger := logrus.StandardLogger()
			oldHooks, oldLevel, oldOut := logger.ReplaceHooks(make(logrus.LevelHooks)), logger.GetLevel(), logger.Out
			stdout := os.Stdout
			r, w, err := os.Pipe()
			require.NoError(t, err)
			os.Stdout = w
			t.Cleanup(func() {
				os.Stdout = stdout
				logger.ReplaceHooks(oldHooks)
				logger.SetLevel(oldLevel)
				logger.SetOutput(oldOut)
				stdLogAdded = false
			})
			stdLogAdded = false
			logger.SetLevel(logrus.InfoLevel)

			SetupHookLogging(tc.hooks)
			for level := range tc.want {
				logrus.WithField("level_under_test", level.String()).Log(level, "message")
			}
			require.NoError(t, w.Close())
			out, err := io.ReadAll(r)
			require.NoError(t, err)
			for level, want := range tc.want {
				require.Equal(t, want, strings.Count(string(out), "level_under_test="+level.String()),
					"%s messages written to stdout:\n%s", level, out)
			}
		})
	}
}
