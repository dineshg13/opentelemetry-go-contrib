// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp // import "go.opentelemetry.io/contrib/otelconf/opamp"

import (
	"context"
	"crypto/tls"
	"net/http"

	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"

	"go.opentelemetry.io/contrib/otelconf"
)

// transport selects the OpAMP wire protocol.
type transport int

const (
	// transportAuto chooses WebSocket unless the server URL begins with "http".
	transportAuto transport = iota
	transportWebSocket
	transportHTTP
)

// config holds the resolved manager configuration. It is populated by Options
// and finalized with defaults in NewManager.
type config struct {
	serverURL        string
	instanceUID      types.InstanceUid
	agentDescription *protobufs.AgentDescription
	header           http.Header
	tlsConfig        *tls.Config
	logger           types.Logger
	store            StateStore
	installer        Installer
	transport        transport
	bootstrap        []byte

	// build constructs an SDK from a parsed declarative configuration. It is
	// injectable so the apply path can be exercised without a real SDK. When
	// nil, NewManager installs the otelconf-backed builder.
	build builderFunc
}

// builderFunc constructs an SDK from a parsed declarative configuration.
type builderFunc func(ctx context.Context, conf *otelconf.OpenTelemetryConfiguration) (*SDK, error)

// Option configures a Manager.
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (f optionFunc) apply(c *config) { f(c) }

// WithServerURL sets the OpAMP server URL. It is required.
func WithServerURL(url string) Option {
	return optionFunc(func(c *config) { c.serverURL = url })
}

// WithInstanceUID sets the agent instance UID reported to the server.
func WithInstanceUID(uid types.InstanceUid) Option {
	return optionFunc(func(c *config) { c.instanceUID = uid })
}

// WithAgentDescription sets the agent description reported to the server.
func WithAgentDescription(descr *protobufs.AgentDescription) Option {
	return optionFunc(func(c *config) { c.agentDescription = descr })
}

// WithHeader sets HTTP headers used when connecting to the server.
func WithHeader(h http.Header) Option {
	return optionFunc(func(c *config) { c.header = h })
}

// WithTLSConfig sets the TLS configuration used when connecting to the server.
func WithTLSConfig(t *tls.Config) Option {
	return optionFunc(func(c *config) { c.tlsConfig = t })
}

// WithLogger sets the logger used by the OpAMP client and the manager.
func WithLogger(l types.Logger) Option {
	return optionFunc(func(c *config) { c.logger = l })
}

// WithStateStore sets the StateStore used to persist remote config status and
// effective configuration. Defaults to an in-memory store.
func WithStateStore(s StateStore) Option {
	return optionFunc(func(c *config) { c.store = s })
}

// WithInstaller sets the Installer used to activate newly built SDKs. Defaults
// to NoopInstaller so that no process-wide globals are mutated unless requested.
func WithInstaller(i Installer) Option {
	return optionFunc(func(c *config) { c.installer = i })
}

// WithBootstrap sets a declarative OpenTelemetry configuration to apply locally
// during [NewSDK], before any remote configuration is received. It gives the
// process working providers immediately and on first connect is reported as the
// effective configuration. It has no effect when using [NewManager] directly.
func WithBootstrap(cfg []byte) Option {
	return optionFunc(func(c *config) { c.bootstrap = cfg })
}

// WithWebSocket forces the WebSocket transport regardless of the server URL.
func WithWebSocket() Option {
	return optionFunc(func(c *config) { c.transport = transportWebSocket })
}

// WithHTTP forces the plain HTTP transport regardless of the server URL.
func WithHTTP() Option {
	return optionFunc(func(c *config) { c.transport = transportHTTP })
}

// noopLogger is the default types.Logger; it discards all output.
type noopLogger struct{}

func (noopLogger) Debugf(context.Context, string, ...any) {}
func (noopLogger) Errorf(context.Context, string, ...any) {}
