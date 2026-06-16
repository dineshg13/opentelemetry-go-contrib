// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp // import "go.opentelemetry.io/contrib/otelconf/opamp"

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"go.opentelemetry.io/contrib/otelconf"
)

// extractConfig selects the configuration body from a remote config message.
// It prefers the conventional empty ("") key and falls back to the sole entry
// when exactly one file is offered. An empty map or an ambiguous multi-file map
// is an error.
func extractConfig(rc *protobufs.AgentRemoteConfig) ([]byte, error) {
	cm := rc.GetConfig().GetConfigMap()
	if len(cm) == 0 {
		return nil, errors.New("opamp: remote config contains no configuration")
	}
	if f, ok := cm[""]; ok {
		return f.GetBody(), nil
	}
	if len(cm) == 1 {
		for _, f := range cm {
			return f.GetBody(), nil
		}
	}
	return nil, fmt.Errorf("opamp: remote config has %d files and no default %q key; cannot choose one", len(cm), "")
}

// otelconfBuilder builds an SDK from a parsed declarative configuration using
// the otelconf package. It is the default builder; tests inject alternatives.
func otelconfBuilder(ctx context.Context, conf *otelconf.OpenTelemetryConfiguration) (*SDK, error) {
	sdk, err := otelconf.NewSDK(
		otelconf.WithContext(ctx),
		otelconf.WithOpenTelemetryConfiguration(*conf),
	)
	if err != nil {
		return nil, fmt.Errorf("opamp: build SDK: %w", err)
	}
	return &SDK{
		TracerProvider: sdk.TracerProvider(),
		MeterProvider:  sdk.MeterProvider(),
		LoggerProvider: sdk.LoggerProvider(),
		Propagator:     sdk.Propagator(),
		Shutdown:       sdk.Shutdown,
	}, nil
}

// ensureServiceInstanceID returns the service.instance.id the SDK resource will
// carry. If the config already sets it, that value wins (the config is
// authoritative for the resource). Otherwise the manager's resolved instance id
// is injected into the config so the telemetry resource and the reported
// AgentDescription carry the same value, as the OpAMP SDK guidelines require.
func (m *Manager) ensureServiceInstanceID(conf *otelconf.OpenTelemetryConfiguration) string {
	if conf.Resource != nil {
		for _, a := range conf.Resource.Attributes {
			if a.Name == keyServiceInstanceID {
				if s, ok := a.Value.(string); ok && s != "" {
					return s
				}
			}
		}
	}
	if m.serviceInstanceID == "" {
		return "" // identity not resolved (e.g. installConfig called directly in tests)
	}
	if conf.Resource == nil {
		conf.Resource = &otelconf.Resource{}
	}
	conf.Resource.Attributes = append(conf.Resource.Attributes, otelconf.AttributeNameValue{
		Name:  keyServiceInstanceID,
		Value: m.serviceInstanceID,
	})
	return m.serviceInstanceID
}

// installConfig parses cfg, builds an SDK, installs it, swaps it in as the
// current SDK, shuts down the previously installed SDK, records cfg as the
// effective configuration, and updates the agent description to match the new
// resource. On any failure the current SDK is left untouched: a parse or build
// error returns before installation, and an install error shuts down the freshly
// built SDK to avoid leaking its resources. It is the server-independent core
// shared by the remote-config and bootstrap paths and is safe to call without an
// OpAMP client.
func (m *Manager) installConfig(ctx context.Context, cfg []byte) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	conf, err := otelconf.ParseYAML(cfg)
	if err != nil {
		return fmt.Errorf("opamp: parse configuration: %w", err)
	}
	instanceID := m.ensureServiceInstanceID(conf)

	next, err := m.build(ctx, conf)
	if err != nil {
		return err
	}

	m.mu.Lock()
	prev := m.current
	m.mu.Unlock()

	if err := m.installer.Install(ctx, *next, prev); err != nil {
		if sErr := next.shutdown(ctx); sErr != nil {
			m.logger.Errorf(ctx, "opamp: shutdown of uninstalled SDK failed: %v", sErr)
		}
		return fmt.Errorf("opamp: install configuration: %w", err)
	}

	m.mu.Lock()
	m.current = next
	m.mu.Unlock()

	// Shut down the previous SDK only after the replacement is installed.
	if err := prev.shutdown(ctx); err != nil {
		m.logger.Errorf(ctx, "opamp: shutdown of replaced SDK failed: %v", err)
	}

	// Persist the server-sent config bytes as the effective configuration. The
	// injected service.instance.id is an identity detail surfaced through the
	// AgentDescription, not part of the declarative config the server controls.
	if err := m.store.SaveEffectiveConfig(ctx, cfg); err != nil {
		m.logger.Errorf(ctx, "opamp: save effective config failed: %v", err)
	}

	// Keep the agent identity in sync with the effective resource.
	m.updateAgentDescription(ctx, conf, instanceID)
	return nil
}

// updateAgentDescription derives the agent description from the effective config
// resource and, when it has changed and an OpAMP client is connected, reports it
// to the server. The latest description is always cached so Start can send it on
// the initial connection.
func (m *Manager) updateAgentDescription(ctx context.Context, conf *otelconf.OpenTelemetryConfiguration, instanceID string) {
	desc := deriveAgentDescription(conf, instanceID, m.cfg.agentDescription)

	m.mu.Lock()
	changed := !proto.Equal(m.lastDesc, desc)
	m.lastDesc = desc
	m.mu.Unlock()

	if changed && m.client != nil {
		if err := m.client.SetAgentDescription(desc); err != nil {
			m.logger.Errorf(ctx, "opamp: set agent description failed: %v", err)
		}
	}
}

// ApplyConfig applies a declarative OpenTelemetry configuration locally, exactly
// as if it had arrived from the OpAMP server: it builds an SDK, installs it
// through the configured [Installer], shuts down the previously installed SDK,
// records the bytes as the effective configuration, and refreshes the agent
// description. On failure the current SDK is left untouched.
//
// It is the way to change configuration without an OpAMP server — useful for
// tests and for local config sources such as a file watch or a SIGHUP reload.
// If an OpAMP client is connected, the new effective configuration is reported
// to the server; no remote config status is recorded, since the change did not
// originate from the server.
func (m *Manager) ApplyConfig(ctx context.Context, cfg []byte) error {
	if err := m.installConfig(ctx, cfg); err != nil {
		return err
	}
	if m.client != nil {
		if err := m.client.UpdateEffectiveConfig(ctx); err != nil {
			m.logger.Errorf(ctx, "opamp: update effective config failed: %v", err)
		}
	}
	return nil
}

// applyRemoteConfig extracts, installs, and records a remote configuration, then
// reports status back to the server. It is the server-independent heart of the
// manager and is safe to call directly in tests without a live OpAMP client.
//
// On any failure the previously installed SDK is left untouched and a FAILED
// status is recorded. On success the new SDK becomes current, the previous SDK
// is shut down, and effective config plus an APPLIED status are recorded.
func (m *Manager) applyRemoteConfig(ctx context.Context, rc *protobufs.AgentRemoteConfig) error {
	hash := rc.GetConfigHash()

	cfg, err := extractConfig(rc)
	if err != nil {
		m.reportStatus(ctx, hash, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error())
		return err
	}

	if err := m.installConfig(ctx, cfg); err != nil {
		// Build or install failed: the current SDK is untouched.
		m.reportStatus(ctx, hash, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error())
		return err
	}

	m.reportStatus(ctx, hash, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "")

	// Report the new effective config to the server. Best effort.
	if m.client != nil {
		if err := m.client.UpdateEffectiveConfig(ctx); err != nil {
			m.logger.Errorf(ctx, "opamp: update effective config failed: %v", err)
		}
	}
	return nil
}

// reportStatus persists the remote config status and, when an OpAMP client is
// present, reports it to the server. Failures are logged, not returned: status
// reporting is best effort and must not mask the apply outcome.
func (m *Manager) reportStatus(ctx context.Context, hash []byte, status protobufs.RemoteConfigStatuses, errMsg string) {
	rcs := &protobufs.RemoteConfigStatus{
		LastRemoteConfigHash: hash,
		Status:               status,
		ErrorMessage:         errMsg,
	}
	if err := m.store.SaveRemoteConfigStatus(ctx, rcs); err != nil {
		m.logger.Errorf(ctx, "opamp: save remote config status failed: %v", err)
	}
	if m.client != nil {
		if err := m.client.SetRemoteConfigStatus(rcs); err != nil {
			m.logger.Errorf(ctx, "opamp: set remote config status failed: %v", err)
		}
	}
}
