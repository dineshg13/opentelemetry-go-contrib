// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp // import "go.opentelemetry.io/contrib/otelconf/opamp"

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"

	"go.opentelemetry.io/otel/log"
	lognoop "go.opentelemetry.io/otel/log/noop"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Manager connects to an OpAMP server and applies the declarative OpenTelemetry
// configuration it receives to the Go SDK. It owns the OpAMP client and the
// currently installed SDK so the previous SDK can be shut down after a
// successful replacement. The zero value is not usable; construct a Manager with
// [NewManager].
type Manager struct {
	cfg       config
	logger    types.Logger
	store     StateStore
	installer Installer
	build     builderFunc

	// applyMu serializes whole config applies (remote, bootstrap, and local
	// ApplyConfig) so an in-flight build/swap cannot interleave with another.
	applyMu sync.Mutex

	mu       sync.Mutex
	current  *SDK
	lastDesc *protobufs.AgentDescription

	// Agent identity, resolved once before Start.
	instanceUID       types.InstanceUid
	serviceInstanceID string
	startTime         int64

	client    client.OpAMPClient
	closeOnce sync.Once

	// newClientFn, when set, overrides client construction. Tests use it to
	// inject a fake OpAMP client.
	newClientFn func() client.OpAMPClient
}

// managerCapabilities is the set of OpAMP capabilities the manager advertises.
func managerCapabilities() protobufs.AgentCapabilities {
	return protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
		protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat
}

// NewManager constructs a Manager. WithServerURL is required; all other options
// have defaults: an in-memory StateStore, a NoopInstaller (no global mutation),
// a no-op logger, transport inferred from the server URL, and an otelconf-backed
// SDK builder.
func NewManager(opts ...Option) (*Manager, error) {
	cfg := config{}
	for _, o := range opts {
		o.apply(&cfg)
	}
	if cfg.serverURL == "" {
		return nil, errors.New("opamp: server URL is required (use WithServerURL)")
	}
	if cfg.logger == nil {
		cfg.logger = noopLogger{}
	}
	if cfg.store == nil {
		cfg.store = NewMemoryStateStore()
	}
	if cfg.installer == nil {
		cfg.installer = NoopInstaller{}
	}
	if cfg.build == nil {
		cfg.build = otelconfBuilder
	}
	return &Manager{
		cfg:       cfg,
		logger:    cfg.logger,
		store:     cfg.store,
		installer: cfg.installer,
		build:     cfg.build,
	}, nil
}

// NewSDK constructs a Manager, applies any bootstrap configuration, and starts
// the OpAMP client in one call. It is the high-level entry point for callers
// that want the SDK to "do the magic": the returned Manager owns the OpAMP
// lifecycle and the installed providers, and Shutdown tears down both.
//
// With WithInstaller(GlobalInstaller{}) the providers are installed into the
// OpenTelemetry globals, so callers should acquire telemetry through the otel
// globals (for example otel.Tracer) so that handles follow remote-config swaps.
// If a bootstrap configuration is provided it is applied before connecting; a
// bootstrap failure aborts NewSDK.
func NewSDK(ctx context.Context, opts ...Option) (*Manager, error) {
	m, err := NewManager(opts...)
	if err != nil {
		return nil, err
	}
	if err := m.resolveIdentity(ctx); err != nil {
		return nil, err
	}
	if len(m.cfg.bootstrap) > 0 {
		if err := m.installConfig(ctx, m.cfg.bootstrap); err != nil {
			return nil, fmt.Errorf("opamp: apply bootstrap configuration: %w", err)
		}
	}
	if err := m.Start(ctx); err != nil {
		// Roll back any bootstrap providers installed above.
		_ = m.Shutdown(context.Background())
		return nil, err
	}
	return m, nil
}

// TracerProvider returns the currently installed TracerProvider, or a no-op
// provider if no configuration has been applied yet. The value is a snapshot;
// re-fetch after a swap. Prefer the OpenTelemetry globals with GlobalInstaller.
func (m *Manager) TracerProvider() trace.TracerProvider {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.TracerProvider != nil {
		return m.current.TracerProvider
	}
	return tracenoop.NewTracerProvider()
}

// MeterProvider returns the currently installed MeterProvider, or a no-op
// provider if no configuration has been applied yet. The value is a snapshot.
func (m *Manager) MeterProvider() metric.MeterProvider {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.MeterProvider != nil {
		return m.current.MeterProvider
	}
	return metricnoop.NewMeterProvider()
}

// LoggerProvider returns the currently installed LoggerProvider, or a no-op
// provider if no configuration has been applied yet. The value is a snapshot.
func (m *Manager) LoggerProvider() log.LoggerProvider {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.LoggerProvider != nil {
		return m.current.LoggerProvider
	}
	return lognoop.NewLoggerProvider()
}

// Propagator returns the currently installed TextMapPropagator, or a no-op
// propagator if no configuration has been applied yet. The value is a snapshot.
func (m *Manager) Propagator() propagation.TextMapPropagator {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && m.current.Propagator != nil {
		return m.current.Propagator
	}
	return noopPropagator{}
}

// noopPropagator is a TextMapPropagator that does nothing. It is the default
// returned by [Manager.Propagator] before any configuration is applied.
type noopPropagator struct{}

func (noopPropagator) Inject(context.Context, propagation.TextMapCarrier) {}

func (noopPropagator) Extract(ctx context.Context, _ propagation.TextMapCarrier) context.Context {
	return ctx
}

func (noopPropagator) Fields() []string { return nil }

// resolveIdentity establishes a stable OpAMP instance UID and the matching
// service.instance.id. Precedence: a caller-supplied WithInstanceUID, then a UID
// previously persisted in the StateStore, then a freshly generated UUIDv7. The
// resolved UID is persisted so it is stable across restarts. It is idempotent.
func (m *Manager) resolveIdentity(ctx context.Context) error {
	if m.instanceUID != (types.InstanceUid{}) {
		return nil
	}

	uid := m.cfg.instanceUID
	if uid == (types.InstanceUid{}) {
		if stored, ok, err := m.store.InstanceUID(ctx); err != nil {
			m.logger.Errorf(ctx, "opamp: load instance uid: %v", err)
		} else if ok {
			uid = stored
		}
	}
	if uid == (types.InstanceUid{}) {
		gen, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("opamp: generate instance uid: %w", err)
		}
		uid = types.InstanceUid(gen)
	}

	if err := m.store.SaveInstanceUID(ctx, uid); err != nil {
		m.logger.Errorf(ctx, "opamp: save instance uid: %v", err)
	}
	m.instanceUID = uid
	m.serviceInstanceID = uuid.UUID(uid).String()
	return nil
}

// newClient creates the OpAMP client for the configured transport. The default
// is WebSocket unless the server URL begins with "http".
func (m *Manager) newClient() client.OpAMPClient {
	switch m.cfg.transport {
	case transportHTTP:
		return client.NewHTTP(m.logger)
	case transportWebSocket:
		return client.NewWebSocket(m.logger)
	default:
		if strings.HasPrefix(m.cfg.serverURL, "http") {
			return client.NewHTTP(m.logger)
		}
		return client.NewWebSocket(m.logger)
	}
}

// Start connects to the OpAMP server and begins applying remote configuration.
// It sets the agent description and capabilities, resumes the last reported
// remote config status from the StateStore, and wires the callbacks that drive
// configuration application. Start does not block waiting for a connection.
func (m *Manager) Start(ctx context.Context) error {
	if m.client != nil {
		return errors.New("opamp: manager already started")
	}
	if err := m.resolveIdentity(ctx); err != nil {
		return err
	}
	c := m.newClient()
	if m.newClientFn != nil {
		c = m.newClientFn()
	}

	// Ensure an agent description is available for the initial connection. If a
	// bootstrap config was applied, updateAgentDescription already derived one;
	// otherwise derive from the SDK defaults and any caller-supplied attributes.
	m.mu.Lock()
	if m.lastDesc == nil {
		m.lastDesc = deriveAgentDescription(nil, m.serviceInstanceID, m.cfg.agentDescription)
	}
	desc := m.lastDesc
	m.mu.Unlock()
	if err := c.SetAgentDescription(desc); err != nil {
		return fmt.Errorf("opamp: set agent description: %w", err)
	}

	// Report a healthy status before advertising ReportsHealth: the client
	// requires health to be set when that capability is enabled.
	m.startTime = time.Now().UnixNano()
	if err := c.SetHealth(&protobufs.ComponentHealth{
		Healthy:           true,
		StartTimeUnixNano: uint64(m.startTime),
	}); err != nil {
		return fmt.Errorf("opamp: set health: %w", err)
	}

	caps := managerCapabilities()
	if err := c.SetCapabilities(&caps); err != nil {
		return fmt.Errorf("opamp: set capabilities: %w", err)
	}

	status, err := m.store.RemoteConfigStatus(ctx)
	if err != nil {
		m.logger.Errorf(ctx, "opamp: load remote config status: %v", err)
	}

	settings := types.StartSettings{
		OpAMPServerURL:     m.cfg.serverURL,
		Header:             m.cfg.header,
		TLSConfig:          m.cfg.tlsConfig,
		InstanceUid:        m.instanceUID,
		RemoteConfigStatus: status,
		Callbacks: types.Callbacks{
			OnConnect: func(ctx context.Context) {
				m.logger.Debugf(ctx, "opamp: connected to server")
			},
			OnConnectFailed: func(ctx context.Context, err error) {
				// The client already logs the connection failure (and retries
				// with exponential backoff) through this same logger, so log at
				// debug here to avoid duplicating it on every retry.
				m.logger.Debugf(ctx, "opamp: connection failed: %v", err)
			},
			OnError: func(ctx context.Context, e *protobufs.ServerErrorResponse) {
				m.logger.Errorf(ctx, "opamp: server error: %s", e.GetErrorMessage())
			},
			OnMessage: func(ctx context.Context, msg *types.MessageData) {
				if msg.RemoteConfig == nil {
					return
				}
				if err := m.applyRemoteConfig(ctx, msg.RemoteConfig); err != nil {
					m.logger.Errorf(ctx, "opamp: apply remote config: %v", err)
				}
			},
			SaveRemoteConfigStatus: func(ctx context.Context, s *protobufs.RemoteConfigStatus) {
				if err := m.store.SaveRemoteConfigStatus(ctx, s); err != nil {
					m.logger.Errorf(ctx, "opamp: save remote config status: %v", err)
				}
			},
			GetEffectiveConfig: m.effectiveConfig,
		},
	}

	// Assign the client before Start so a remote config delivered during the
	// initial handshake can be reported back to the server.
	m.client = c
	if err := c.Start(ctx, settings); err != nil {
		m.client = nil
		return fmt.Errorf("opamp: start client: %w", err)
	}
	return nil
}

// effectiveConfig returns the current effective configuration from the
// StateStore in OpAMP form, keyed by the conventional empty ("") name.
func (m *Manager) effectiveConfig(ctx context.Context) (*protobufs.EffectiveConfig, error) {
	b, err := m.store.EffectiveConfig(ctx)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return &protobufs.EffectiveConfig{}, nil
	}
	return &protobufs.EffectiveConfig{
		ConfigMap: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigFile{
				"": {Body: b, ContentType: "text/yaml"},
			},
		},
	}, nil
}

// Shutdown stops the OpAMP client and shuts down the currently installed SDK. It
// is idempotent: repeated calls after the first are no-ops. Stopping the client
// first guarantees no further configuration is applied during shutdown.
func (m *Manager) Shutdown(ctx context.Context) error {
	var err error
	m.closeOnce.Do(func() {
		if m.client != nil {
			if sErr := m.client.Stop(ctx); sErr != nil {
				err = errors.Join(err, fmt.Errorf("opamp: stop client: %w", sErr))
			}
		}
		m.mu.Lock()
		cur := m.current
		m.current = nil
		m.mu.Unlock()
		if sErr := cur.shutdown(ctx); sErr != nil {
			err = errors.Join(err, fmt.Errorf("opamp: shutdown SDK: %w", sErr))
		}
	})
	return err
}
