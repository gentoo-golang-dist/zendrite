// Copyright 2026 Patrick Schratz
//
// SPDX-License-Identifier: AGPL-3.0-only OR LicenseRef-Element-Commercial
// Please see LICENSE files in the repository root for full details.

package consumers

import (
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/jetstream"
	"codefloe.com/pat-s/zendrite/test"
	"codefloe.com/pat-s/zendrite/test/testrig"
)

type activeOwnership struct{}

func (activeOwnership) Generation() int64 { return 1 }
func (activeOwnership) Identity() string  { return "candidate-a" }
func (activeOwnership) Valid() error      { return nil }
func (activeOwnership) Retired() bool     { return false }

// TestAppserviceConsumersStartUnderHA covers issue #658: under HA the legacy
// consumer cleanup made every configured appservice panic during activation.
// Startup must succeed whether or not the legacy durables exist, and an existing
// one is left in place for an offline migration.
func TestAppserviceConsumersStartUnderHA(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "fresh"
		if legacy {
			name = "legacy durables present"
		}
		t.Run(name, func(t *testing.T) {
			cfg, processCtx, closeDB := testrig.CreateConfig(t, test.DBTypeSQLite)
			defer closeDB()
			cfg.AppServiceAPI.Derived.ApplicationServices = []config.ApplicationService{
				{ID: "meowlnir", URL: "http://meowlnir.test"},
				{ID: "silent"}, // no URL and no ephemeral data
			}
			natsInstance := &jetstream.NATSInstance{}
			js, _ := natsInstance.Prepare(processCtx, &cfg.Global.JetStream)
			jsCfg := cfg.Global.JetStream

			roomEvents := jsCfg.Prefixed(jetstream.OutputRoomEvent)
			legacyRoom := jsCfg.Durable("Appservice_"+jetstream.Tokenise("meowlnir")) + "Pull"
			typing := jsCfg.Prefixed(jetstream.OutputTypingEvent)
			legacyTyping := jsCfg.Durable("AppserviceTyping_"+jetstream.Tokenise("silent")) + "Pull"
			if legacy {
				for stream, durable := range map[string]string{roomEvents: legacyRoom, typing: legacyTyping} {
					_, err := js.AddConsumer(stream, &nats.ConsumerConfig{Durable: durable, AckPolicy: nats.AckExplicitPolicy})
					require.NoError(t, err)
				}
			}

			protected := jetstream.Protected(js, activeOwnership{})
			require.NoError(t, NewOutputRoomEventConsumer(processCtx, &cfg.AppServiceAPI, protected, nil).Start())
			require.NoError(t, NewEphemeralConsumer(processCtx, &cfg.AppServiceAPI, protected, nil).Start())

			for stream, durable := range map[string]string{roomEvents: legacyRoom, typing: legacyTyping} {
				_, err := js.ConsumerInfo(stream, durable)
				if legacy {
					require.NoError(t, err, "HA must not delete %q", durable)
				} else {
					require.ErrorIs(t, err, nats.ErrConsumerNotFound)
				}
			}
			processCtx.ShutdownZendrite()
			processCtx.WaitForComponentsToFinish()
		})
	}
}
