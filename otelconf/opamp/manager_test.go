// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/contrib/otelconf"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

const applied = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
const failed = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED

// validYAML is a minimal config that parses, for tests that inject the builder
// and only care about the install/apply flow, not the SDK contents.
const validYAML = "file_format: \"0.3\"\n"

// remoteConfig builds a single-file AgentRemoteConfig keyed by "".
func remoteConfig(body string) *protobufs.AgentRemoteConfig {
	return &protobufs.AgentRemoteConfig{
		Config: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigFile{
				"": {Body: []byte(body)},
			},
		},
		ConfigHash: []byte("hash-1"),
	}
}

func TestStartupConfigResumesServerConfigElseBootstrap(t *testing.T) {
	ctx := context.Background()

	// First run / server never engaged: use the bootstrap config.
	m := newTestManager(t, WithBootstrap([]byte("boot")))
	cfg, err := m.startupConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, "boot", string(cfg), "first run should apply the bootstrap")

	// Server has applied a config before (status with hash persisted): resume
	// the last effective config instead of the bootstrap.
	store := NewMemoryStateStore()
	require.NoError(t, store.SaveRemoteConfigStatus(ctx, &protobufs.RemoteConfigStatus{
		LastRemoteConfigHash: []byte("hash-1"),
		Status:               applied,
	}))
	require.NoError(t, store.SaveEffectiveConfig(ctx, []byte("server-config")))

	m2 := newTestManager(t, WithBootstrap([]byte("boot")), WithStateStore(store))
	cfg2, err := m2.startupConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, "server-config", string(cfg2), "restart should resume the server config")
}

// newTestManager builds a manager with no OpAMP client (m.client stays nil) so
// applyRemoteConfig can be driven directly without a live server.
func newTestManager(t *testing.T, opts ...Option) *Manager {
	t.Helper()
	opts = append([]Option{WithServerURL("ws://test")}, opts...)
	m, err := NewManager(opts...)
	require.NoError(t, err)
	return m
}

func TestExtractConfig(t *testing.T) {
	tests := []struct {
		name    string
		rc      *protobufs.AgentRemoteConfig
		want    string
		wantErr bool
	}{
		{
			name: "empty key preferred",
			rc: &protobufs.AgentRemoteConfig{Config: &protobufs.AgentConfigMap{
				ConfigMap: map[string]*protobufs.AgentConfigFile{
					"":      {Body: []byte("chosen")},
					"other": {Body: []byte("ignored")},
				},
			}},
			want: "chosen",
		},
		{
			name: "single entry fallback",
			rc: &protobufs.AgentRemoteConfig{Config: &protobufs.AgentConfigMap{
				ConfigMap: map[string]*protobufs.AgentConfigFile{
					"only": {Body: []byte("solo")},
				},
			}},
			want: "solo",
		},
		{
			name: "ambiguous multi-file",
			rc: &protobufs.AgentRemoteConfig{Config: &protobufs.AgentConfigMap{
				ConfigMap: map[string]*protobufs.AgentConfigFile{
					"a": {Body: []byte("a")},
					"b": {Body: []byte("b")},
				},
			}},
			wantErr: true,
		},
		{
			name:    "empty map",
			rc:      &protobufs.AgentRemoteConfig{Config: &protobufs.AgentConfigMap{}},
			wantErr: true,
		},
		{
			name:    "nil config",
			rc:      &protobufs.AgentRemoteConfig{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractConfig(tt.rc)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestApplySuccess(t *testing.T) {
	store := NewMemoryStateStore()
	m := newTestManager(t, WithStateStore(store))

	body, err := os.ReadFile(filepath.Join("testdata", "basic.yaml"))
	require.NoError(t, err)

	require.NoError(t, m.applyRemoteConfig(context.Background(), remoteConfig(string(body))))

	// Status APPLIED with the server-provided hash.
	status, err := store.RemoteConfigStatus(context.Background())
	require.NoError(t, err)
	require.NotNil(t, status)
	assert.Equal(t, applied, status.GetStatus())
	assert.Equal(t, []byte("hash-1"), status.GetLastRemoteConfigHash())
	assert.Empty(t, status.GetErrorMessage())

	// Effective config saved verbatim.
	eff, err := store.EffectiveConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, body, eff)

	// An SDK is now installed and owned by the manager.
	require.NotNil(t, m.current)
	require.NoError(t, m.Shutdown(context.Background()))
}

func TestApplyParseFailureReportsFailed(t *testing.T) {
	store := NewMemoryStateStore()
	m := newTestManager(t, WithStateStore(store))

	err := m.applyRemoteConfig(context.Background(), remoteConfig(":::not valid yaml:::"))
	require.Error(t, err)

	status, err2 := store.RemoteConfigStatus(context.Background())
	require.NoError(t, err2)
	require.NotNil(t, status)
	assert.Equal(t, failed, status.GetStatus())
	assert.NotEmpty(t, status.GetErrorMessage())

	// No SDK was installed and no effective config saved.
	assert.Nil(t, m.current)
	eff, err3 := store.EffectiveConfig(context.Background())
	require.NoError(t, err3)
	assert.Nil(t, eff)
}

func TestApplyBuildFailureKeepsPrevious(t *testing.T) {
	store := NewMemoryStateStore()
	m := newTestManager(t, WithStateStore(store))

	// Seed a current SDK that records whether it is shut down.
	var prevShutdown atomic.Int32
	prev := &SDK{Shutdown: func(context.Context) error { prevShutdown.Add(1); return nil }}
	m.current = prev

	m.build = func(context.Context, *otelconf.OpenTelemetryConfiguration) (*SDK, error) {
		return nil, errors.New("boom")
	}

	err := m.applyRemoteConfig(context.Background(), remoteConfig(validYAML))
	require.Error(t, err)

	// Previous SDK is untouched.
	assert.Same(t, prev, m.current)
	assert.Equal(t, int32(0), prevShutdown.Load())

	status, _ := store.RemoteConfigStatus(context.Background())
	assert.Equal(t, failed, status.GetStatus())
}

func TestPreviousShutdownAfterSuccessfulReplacement(t *testing.T) {
	m := newTestManager(t)

	var aShutdown, bShutdown atomic.Int32
	sdkA := &SDK{Shutdown: func(context.Context) error { aShutdown.Add(1); return nil }}
	sdkB := &SDK{Shutdown: func(context.Context) error { bShutdown.Add(1); return nil }}

	builds := []*SDK{sdkA, sdkB}
	var idx int
	m.build = func(context.Context, *otelconf.OpenTelemetryConfiguration) (*SDK, error) {
		sdk := builds[idx]
		idx++
		return sdk, nil
	}

	// First apply installs A.
	require.NoError(t, m.applyRemoteConfig(context.Background(), remoteConfig(validYAML)))
	assert.Same(t, sdkA, m.current)
	assert.Equal(t, int32(0), aShutdown.Load())

	// Second apply installs B and shuts down A exactly once.
	require.NoError(t, m.applyRemoteConfig(context.Background(), remoteConfig(validYAML)))
	assert.Same(t, sdkB, m.current)
	assert.Equal(t, int32(1), aShutdown.Load())
	assert.Equal(t, int32(0), bShutdown.Load())
}

// failingInstaller always fails to install.
type failingInstaller struct{}

func (failingInstaller) Install(context.Context, SDK, *SDK) error {
	return errors.New("install rejected")
}

func TestInstallFailureShutsDownNextAndKeepsCurrent(t *testing.T) {
	store := NewMemoryStateStore()
	m := newTestManager(t, WithStateStore(store), WithInstaller(failingInstaller{}))

	var prevShutdown, nextShutdown atomic.Int32
	prev := &SDK{Shutdown: func(context.Context) error { prevShutdown.Add(1); return nil }}
	m.current = prev

	next := &SDK{Shutdown: func(context.Context) error { nextShutdown.Add(1); return nil }}
	m.build = func(context.Context, *otelconf.OpenTelemetryConfiguration) (*SDK, error) { return next, nil }

	err := m.applyRemoteConfig(context.Background(), remoteConfig(validYAML))
	require.Error(t, err)

	// The freshly built SDK is shut down to avoid a leak; current is unchanged.
	assert.Equal(t, int32(1), nextShutdown.Load())
	assert.Same(t, prev, m.current)
	assert.Equal(t, int32(0), prevShutdown.Load())

	status, _ := store.RemoteConfigStatus(context.Background())
	assert.Equal(t, failed, status.GetStatus())
}

func TestGlobalInstallerSetsPropagator(t *testing.T) {
	next := SDK{Propagator: propagation.TraceContext{}}
	require.NoError(t, GlobalInstaller{}.Install(context.Background(), next, nil))

	// The global propagator now exposes the TraceContext fields.
	assert.Contains(t, otel.GetTextMapPropagator().Fields(), "traceparent")

	// Installing an SDK with nil fields must not panic.
	assert.NoError(t, GlobalInstaller{}.Install(context.Background(), SDK{}, nil))
}

func TestInstallConfigBootstrapPath(t *testing.T) {
	store := NewMemoryStateStore()
	m := newTestManager(t, WithStateStore(store))

	body, err := os.ReadFile(filepath.Join("testdata", "basic.yaml"))
	require.NoError(t, err)

	// installConfig is the bootstrap path: it installs and records effective
	// config but does not report a remote config status (no server hash).
	require.NoError(t, m.installConfig(context.Background(), body))

	require.NotNil(t, m.current)
	eff, err := store.EffectiveConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, body, eff)

	status, err := store.RemoteConfigStatus(context.Background())
	require.NoError(t, err)
	assert.Nil(t, status, "bootstrap must not record a remote config status")

	require.NoError(t, m.Shutdown(context.Background()))
}

func TestProviderAccessorsDefaultToNoop(t *testing.T) {
	m := newTestManager(t)

	// No configuration applied yet: accessors must return non-nil no-ops.
	assert.NotNil(t, m.TracerProvider())
	assert.NotNil(t, m.MeterProvider())
	assert.NotNil(t, m.LoggerProvider())
	assert.NotNil(t, m.Propagator())

	// After applying a config, accessors expose the installed providers.
	body, err := os.ReadFile(filepath.Join("testdata", "basic.yaml"))
	require.NoError(t, err)
	require.NoError(t, m.installConfig(context.Background(), body))
	assert.Same(t, m.current.TracerProvider, m.TracerProvider())
	require.NoError(t, m.Shutdown(context.Background()))
}

func TestApplyConfigSwapsConfigWithoutServer(t *testing.T) {
	store := NewMemoryStateStore()
	m := newTestManager(t, WithStateStore(store)) // m.client is nil: no server

	var aShutdown atomic.Int32
	sdkA := &SDK{Shutdown: func(context.Context) error { aShutdown.Add(1); return nil }}
	sdkB := &SDK{Shutdown: func(context.Context) error { return nil }}
	builds := []*SDK{sdkA, sdkB}
	var idx int
	m.build = func(context.Context, *otelconf.OpenTelemetryConfiguration) (*SDK, error) {
		sdk := builds[idx]
		idx++
		return sdk, nil
	}

	cfgA := []byte("file_format: \"0.3\"\nresource:\n  attributes:\n    - name: service.name\n      value: svc-a\n")
	cfgB := []byte("file_format: \"0.3\"\nresource:\n  attributes:\n    - name: service.name\n      value: svc-b\n")

	require.NoError(t, m.ApplyConfig(context.Background(), cfgA))
	assert.Same(t, sdkA, m.current)

	// Changing config swaps the SDK and shuts the previous one down.
	require.NoError(t, m.ApplyConfig(context.Background(), cfgB))
	assert.Same(t, sdkB, m.current)
	assert.Equal(t, int32(1), aShutdown.Load(), "previous SDK shut down after swap")

	// Effective config reflects the latest applied bytes.
	eff, err := store.EffectiveConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, cfgB, eff)

	// A local apply records no remote config status.
	status, err := store.RemoteConfigStatus(context.Background())
	require.NoError(t, err)
	assert.Nil(t, status)
}

func TestNewManagerRequiresServerURL(t *testing.T) {
	_, err := NewManager()
	assert.Error(t, err)
}

func TestShutdownIdempotent(t *testing.T) {
	m := newTestManager(t)
	var n atomic.Int32
	m.current = &SDK{Shutdown: func(context.Context) error { n.Add(1); return nil }}

	require.NoError(t, m.Shutdown(context.Background()))
	require.NoError(t, m.Shutdown(context.Background()))
	assert.Equal(t, int32(1), n.Load())
}
