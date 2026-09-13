package jetstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsclient "github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"

	"codefloe.com/pat-s/zendrite/setup/config"
	"codefloe.com/pat-s/zendrite/setup/process"
)

type NATSInstance struct {
	*natsserver.Server
	nc *natsclient.Conn
	js natsclient.JetStreamContext
}

var natsLock sync.Mutex

func DeleteAllStreams(js natsclient.JetStreamContext, cfg *config.JetStream) {
	for _, stream := range streams { // streams are defined in streams.go
		name := cfg.Prefixed(stream.Name)
		_ = js.DeleteStream(name)
	}
}

func (s *NATSInstance) Prepare(process *process.ProcessContext, cfg *config.JetStream) (natsclient.JetStreamContext, *natsclient.Conn) {
	natsLock.Lock()
	defer natsLock.Unlock()
	var err error

	// If an existing connection exists, return it.
	if s.nc != nil && s.js != nil {
		return s.js, s.nc
	}

	// For connecting to an external NATS server.
	if len(cfg.Addresses) > 0 {
		s.js, s.nc = setupNATS(process, cfg, nil)
		process.Defer(s.nc.Close)
		return s.js, s.nc
	}

	if len(cfg.Addresses) == 0 && s.Server == nil {
		opts := &natsserver.Options{
			ServerName:      "monolith",
			DontListen:      true,
			JetStream:       true,
			StoreDir:        string(cfg.StoragePath),
			NoSystemAccount: true,
			MaxPayload:      8 * 1024 * 1024, //nolint:mnd
			NoSigs:          true,
			NoLog:           cfg.NoLog,
			SyncAlways:      true,
		}
		if s.Server, err = natsserver.NewServer(opts); err != nil {
			panic(err)
		}
		if !cfg.NoLog {
			s.SetLogger(NewLogAdapter(), opts.Debug, opts.Trace)
		}
		go s.Start()
		process.Defer(func() { s.Shutdown(); s.WaitForShutdown() })
		if !s.ReadyForConnections(time.Second * 60) { //nolint:mnd
			logrus.Fatalln("NATS did not start in time, shutting down")
			process.ShutdownZendrite()
			s.Shutdown()
			s.WaitForShutdown()
			return nil, nil
		}
	}

	// No existing process connection, create a new one.
	if s.nc, err = natsclient.Connect("", natsclient.InProcessServer(s.Server)); err != nil {
		logrus.Fatalln("Failed to create NATS client")
	}
	s.js, s.nc = setupNATS(process, cfg, s.nc)
	process.Defer(s.nc.Close)
	return s.js, s.nc
}

func setupNATS(process *process.ProcessContext, cfg *config.JetStream, nc *natsclient.Conn) (natsclient.JetStreamContext, *natsclient.Conn) {
	jsOpts := []natsclient.JSOpt{}
	if cfg.JetStreamDomain != "" {
		jsOpts = append(jsOpts, natsclient.Domain(cfg.JetStreamDomain))
	}

	if nc == nil {
		opts, err := natsConnectOptions(process, cfg, jsOpts)
		if err != nil {
			logrus.WithError(err).Fatal("Unable to configure NATS connection")
		}

		nc, err = natsclient.Connect(strings.Join(cfg.Addresses, ","), opts...)
		if err != nil {
			if cfg.Matrix != nil && cfg.Matrix.HA.Enabled {
				logrus.Panic("Unable to connect to external NATS in HA mode; check endpoints and credentials")
			}
			if isAuthError(err) {
				logrus.WithError(err).Fatal("NATS authentication failed")
			}
			logrus.WithError(err).Panic("Unable to connect to NATS")
		}
	}

	js, err := nc.JetStream(jsOpts...)
	if err != nil {
		logrus.WithError(err).Panic("Unable to get JetStream context")
	}
	checkAndConfigureStreams(process, cfg, js)
	if cfg.Matrix != nil && cfg.Matrix.HA.Enabled {
		js = protectedJetStream{js}
	}
	return js, nc
}

// natsConnectOptions builds the NATS client options used for external
// JetStream connections. When a credentials file is configured, the file is
// validated upfront so that missing or malformed credentials produce a clear
// error at startup. The NATS client re-reads the credentials file on each
// reconnect, so rotating the file contents does not require a process restart.
func natsConnectOptions(process *process.ProcessContext, cfg *config.JetStream, jsOpts []natsclient.JSOpt) ([]natsclient.Option, error) {
	opts := []natsclient.Option{
		natsclient.Name("Zendrite"),
		natsclient.MaxReconnects(-1), // Try forever
		natsclient.ReconnectJitter(time.Second, time.Second),
		natsclient.ReconnectWait(time.Second * 10), //nolint:mnd
		natsclient.ReconnectHandler(func(c *natsclient.Conn) {
			process.Do(func() {
				js, jerr := c.JetStream(jsOpts...)
				if jerr != nil {
					logrus.WithError(jerr).Panic("Unable to get JetStream context in reconnect handler")
					return
				}
				checkAndConfigureStreams(process, cfg, js)
			})
		}),
	}

	if cfg.DisableTLSValidation {
		opts = append(opts, natsclient.Secure(&tls.Config{
			InsecureSkipVerify: true,
		}))
	}

	if string(cfg.Credentials) != "" {
		if err := validateCredentials(string(cfg.Credentials)); err != nil {
			return nil, err
		}
		opts = append(opts, natsclient.UserCredentials(string(cfg.Credentials)))
	}

	return opts, nil
}

// validateCredentials checks that the supplied NATS .creds file can be read
// and that both the JWT and the nkey seed can be used. This mirrors the
// validation performed by nats.UserCredentials and additionally exercises the
// signature callback so that malformed seeds are caught at startup.
func validateCredentials(path string) error {
	opt := natsclient.UserCredentials(path)
	var opts natsclient.Options
	if err := opt(&opts); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("NATS credentials file not found: %w", err)
		}
		return fmt.Errorf("NATS credentials file %q is invalid: %w", path, err)
	}
	// The UserCredentials option only smoke-tests the user JWT callback. Also
	// exercise the signature callback so that a missing or corrupt nkey seed
	// fails fast rather than during the first NATS reconnect.
	if _, err := opts.SignatureCB([]byte("zendrite-credential-check")); err != nil {
		return fmt.Errorf("NATS credentials file %q is invalid: %w", path, err)
	}
	return nil
}

// isAuthError reports whether a NATS connection error is an authentication or
// authorization failure.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, natsclient.ErrAuthorization) ||
		errors.Is(err, natsclient.ErrAuthExpired) ||
		errors.Is(err, natsclient.ErrAuthRevoked) ||
		errors.Is(err, natsclient.ErrAccountAuthExpired)
}

func checkAndConfigureStreams(process *process.ProcessContext, cfg *config.JetStream, js natsclient.JetStreamContext) {
	if err := configureStreams(process, cfg, js); err != nil {
		logrus.WithError(err).Panic("Unable to establish JetStream storage")
	}
	// Removing interest consumers can discard their outstanding messages.
	// HA requires an explicit offline migration of legacy consumers instead.
	if cfg.Matrix != nil && cfg.Matrix.HA.Enabled {
		return
	}

	// Clean up old consumers so that interest-based consumers do the
	// right thing.
	for stream, consumers := range map[string][]string{
		OutputClientData:        {"SyncAPIClientAPIConsumer"},
		OutputReceiptEvent:      {"SyncAPIEDUServerReceiptConsumer", "FederationAPIEDUServerConsumer"},
		OutputSendToDeviceEvent: {"SyncAPIEDUServerSendToDeviceConsumer", "FederationAPIEDUServerConsumer"},
		OutputTypingEvent:       {"SyncAPIEDUServerTypingConsumer", "FederationAPIEDUServerConsumer"},
		OutputRoomEvent:         {"AppserviceRoomserverConsumer"},
		OutputStreamEvent:       {"UserAPISyncAPIStreamEventConsumer"},
		OutputReadUpdate:        {"UserAPISyncAPIReadUpdateConsumer"},
	} {
		streamName := cfg.Matrix.JetStream.Prefixed(stream)
		for _, consumer := range consumers {
			consumerName := cfg.Matrix.JetStream.Prefixed(consumer) + "Pull"
			consumerInfo, err := js.ConsumerInfo(streamName, consumerName)
			if err != nil || consumerInfo == nil {
				continue
			}
			if err = js.DeleteConsumer(streamName, consumerName); err != nil {
				logrus.WithError(err).Errorf("Unable to clean up old consumer %q for stream %q", consumer, stream)
			}
		}
	}
}

// configureStreams never deletes streams and preserves operator-supplied limits.
// HA rejects retention changes because even a successful update can discard data.
//
//nolint:contextcheck,nolintlint // ProcessContext owns the lifetime; this suppresses transitive callgraph facts, which nolintlint cannot track.
func configureStreams(process *process.ProcessContext, cfg *config.JetStream, js natsclient.JetStreamContext) error {
	ha := cfg.Matrix != nil && cfg.Matrix.HA.Enabled
	for _, template := range streams {
		desired := *template
		desired.Name = cfg.Prefixed(template.Name)
		desired.Subjects = []string{desired.Name, desired.Name + ".>"}
		desired.Replicas = cfg.Replicas
		if desired.Replicas == 0 {
			desired.Replicas = 1
		}
		if ha {
			desired.Storage = natsclient.FileStorage
		} else if cfg.InMemory {
			desired.Storage = natsclient.MemoryStorage
		}
		info, err := js.StreamInfo(desired.Name, natsclient.Context(process.Context()))
		if err != nil && !errors.Is(err, natsclient.ErrStreamNotFound) {
			return fmt.Errorf("inspect stream %s: %w", desired.Name, err)
		}
		if info == nil {
			info, err = js.AddStream(&desired, natsclient.Context(process.Context()))
			if err != nil && !ha && desired.Storage == natsclient.FileStorage {
				desired.Storage = natsclient.MemoryStorage
				info, err = js.AddStream(&desired, natsclient.Context(process.Context()))
				if err == nil {
					process.Degraded(fmt.Errorf("stream %s is running in memory", desired.Name))
				}
			}
		} else {
			current := info.Config
			if ha && (current.Storage != desired.Storage || current.Retention != desired.Retention || current.MaxAge != desired.MaxAge || !slices.Equal(current.Subjects, desired.Subjects)) {
				return fmt.Errorf("stream %s requires an offline storage/retention/subject migration; refusing to discard queued data", desired.Name)
			}
			if current.Storage != desired.Storage || current.Retention != desired.Retention || current.MaxAge != desired.MaxAge || !slices.Equal(current.Subjects, desired.Subjects) || current.Replicas != desired.Replicas {
				current.Storage = desired.Storage
				current.Retention = desired.Retention
				current.MaxAge = desired.MaxAge
				current.Subjects = desired.Subjects
				current.Replicas = desired.Replicas
				info, err = js.UpdateStream(&current, natsclient.Context(process.Context()))
			}
		}
		if err != nil {
			return fmt.Errorf("configure stream %s without deleting data: %w", desired.Name, err)
		}
		if info == nil || info.Config.Storage != desired.Storage || info.Config.Replicas != desired.Replicas {
			return fmt.Errorf("stream %s did not establish requested storage and replication", desired.Name)
		}
		if ha {
			if err = waitForReplicas(process.Context(), js, desired.Name, desired.Replicas); err != nil {
				return err
			}
			if err = validateConsumers(process.Context(), js, desired.Name, desired.Replicas); err != nil {
				return err
			}
		}
	}
	return nil
}

// protectedJetStream disallows destructive compatibility repairs in HA.
type protectedJetStream struct{ natsclient.JetStreamContext }

func (protectedJetStream) DeleteStream(string, ...natsclient.JSOpt) error {
	return fmt.Errorf("HA refuses stream deletion; migrate offline")
}

func (protectedJetStream) DeleteConsumer(string, string, ...natsclient.JSOpt) error {
	return fmt.Errorf("HA refuses consumer deletion; migrate offline")
}

func waitForReplicas(ctx context.Context, js natsclient.JetStreamContext, name string, replicas int) error {
	const replicationTimeout = 30 * time.Second
	ctx, cancel := context.WithTimeout(ctx, replicationTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := js.StreamInfo(name, natsclient.Context(ctx))
		if err != nil {
			return fmt.Errorf("inspect replication for %s: %w", name, err)
		}
		healthy := info.Config.Replicas == replicas && info.Cluster != nil && info.Cluster.Leader != "" && len(info.Cluster.Replicas) == replicas-1
		if healthy {
			for _, replica := range info.Cluster.Replicas {
				healthy = healthy && replica.Current && !replica.Offline && replica.Lag == 0
			}
		}
		if healthy {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stream %s replication is not ready: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

func validateConsumers(ctx context.Context, js natsclient.JetStreamContext, stream string, replicas int) error {
	// ConsumerNames hides API errors; use StreamInfo's count to detect incomplete enumeration.
	info, err := js.StreamInfo(stream, natsclient.Context(ctx))
	if err != nil {
		return err
	}
	count := 0
	for name := range js.ConsumerNames(stream, natsclient.Context(ctx)) {
		consumer, readErr := js.ConsumerInfo(stream, name, natsclient.Context(ctx))
		if readErr != nil {
			return readErr
		}
		count++
		cfg := consumer.Config
		if cfg.MemoryStorage || (cfg.Replicas != 0 && cfg.Replicas != replicas) || cfg.DeliverSubject != "" || cfg.AckPolicy != natsclient.AckExplicitPolicy {
			return fmt.Errorf("consumer %s on %s requires an offline durability/acknowledgement migration", name, stream)
		}
	}
	if count != info.State.Consumers {
		return fmt.Errorf("unable to inspect every consumer on %s", stream)
	}
	return ctx.Err()
}
