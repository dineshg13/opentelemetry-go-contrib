// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp // import "go.opentelemetry.io/contrib/otelconf/opamp"

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-telemetry/opamp-go/protobufs"

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

// otelconfBuilder builds an SDK from declarative configuration bytes using the
// otelconf package. It is the default builder; tests inject alternatives.
func otelconfBuilder(ctx context.Context, cfg []byte) (*SDK, error) {
	conf, err := otelconf.ParseYAML(cfg)
	if err != nil {
		return nil, fmt.Errorf("opamp: parse configuration: %w", err)
	}
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

// applyRemoteConfig builds, installs, and records a remote configuration. It is
// the server-independent heart of the manager and is safe to call directly in
// tests without a live OpAMP client.
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

	next, err := m.build(ctx, cfg)
	if err != nil {
		// Do not touch the current SDK: construction failed.
		m.reportStatus(ctx, hash, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error())
		return err
	}

	m.mu.Lock()
	prev := m.current
	m.mu.Unlock()

	if err := m.installer.Install(ctx, *next, prev); err != nil {
		// Installation failed: shut down the SDK we just built to avoid leaking
		// its resources, and keep the current one active.
		if sErr := next.shutdown(ctx); sErr != nil {
			m.logger.Errorf(ctx, "opamp: shutdown of uninstalled SDK failed: %v", sErr)
		}
		err = fmt.Errorf("opamp: install configuration: %w", err)
		m.reportStatus(ctx, hash, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error())
		return err
	}

	m.mu.Lock()
	m.current = next
	m.mu.Unlock()

	// Shut down the previous SDK only after the replacement is installed.
	if err := prev.shutdown(ctx); err != nil {
		m.logger.Errorf(ctx, "opamp: shutdown of replaced SDK failed: %v", err)
	}

	if err := m.store.SaveEffectiveConfig(ctx, cfg); err != nil {
		m.logger.Errorf(ctx, "opamp: save effective config failed: %v", err)
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
