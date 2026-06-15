// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp // import "go.opentelemetry.io/contrib/otelconf/opamp"

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
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

	mu      sync.Mutex
	current *SDK

	client    client.OpAMPClient
	closeOnce sync.Once
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
	c := m.newClient()

	if m.cfg.agentDescription != nil {
		if err := c.SetAgentDescription(m.cfg.agentDescription); err != nil {
			return fmt.Errorf("opamp: set agent description: %w", err)
		}
	}

	caps := protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
		protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig
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
		InstanceUid:        m.cfg.instanceUID,
		RemoteConfigStatus: status,
		Callbacks: types.Callbacks{
			OnConnect: func(ctx context.Context) {
				m.logger.Debugf(ctx, "opamp: connected to server")
			},
			OnConnectFailed: func(ctx context.Context, err error) {
				m.logger.Errorf(ctx, "opamp: connection failed: %v", err)
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
