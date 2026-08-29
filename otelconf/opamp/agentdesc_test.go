// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"sync"
	"testing"

	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/contrib/otelconf"
)

func findKV(kvs []*protobufs.KeyValue, key string) (*protobufs.KeyValue, bool) {
	for _, kv := range kvs {
		if kv.GetKey() == key {
			return kv, true
		}
	}
	return nil, false
}

func identifyingValue(t *testing.T, d *protobufs.AgentDescription, key string) string {
	t.Helper()
	kv, ok := findKV(d.GetIdentifyingAttributes(), key)
	require.Truef(t, ok, "identifying attribute %q not found", key)
	return kv.GetValue().GetStringValue()
}

func resourceConfig(attrs map[string]string) *otelconf.OpenTelemetryConfiguration {
	res := &otelconf.Resource{}
	for k, v := range attrs {
		res.Attributes = append(res.Attributes, otelconf.AttributeNameValue{Name: k, Value: v})
	}
	return &otelconf.OpenTelemetryConfiguration{Resource: res}
}

func TestDeriveAgentDescriptionSplit(t *testing.T) {
	conf := resourceConfig(map[string]string{
		keyServiceName:               "calendar",
		keyServiceNamespace:          "shop",
		keyDeploymentEnvironmentName: "prod",
	})

	d := deriveAgentDescription(conf, "inst-1", nil)

	// service.* and telemetry.sdk.name are identifying and match the resource.
	assert.Equal(t, "calendar", identifyingValue(t, d, keyServiceName))
	assert.Equal(t, "shop", identifyingValue(t, d, keyServiceNamespace))
	assert.Equal(t, "inst-1", identifyingValue(t, d, keyServiceInstanceID))
	assert.Equal(t, "opentelemetry", identifyingValue(t, d, keyTelemetrySDKName))

	// deployment.environment.name and other telemetry.sdk.* are non-identifying.
	_, ok := findKV(d.GetNonIdentifyingAttributes(), keyDeploymentEnvironmentName)
	assert.True(t, ok, "deployment.environment.name should be non-identifying")
	_, ok = findKV(d.GetNonIdentifyingAttributes(), "telemetry.sdk.language")
	assert.True(t, ok, "telemetry.sdk.language should be non-identifying")

	// Identity keys must not leak into non-identifying.
	_, ok = findKV(d.GetNonIdentifyingAttributes(), keyServiceName)
	assert.False(t, ok)
	_, ok = findKV(d.GetNonIdentifyingAttributes(), keyTelemetrySDKName)
	assert.False(t, ok, "telemetry.sdk.name must not also be non-identifying")
}

func TestDeriveAgentDescriptionInjectsInstanceID(t *testing.T) {
	conf := resourceConfig(map[string]string{keyServiceName: "svc"})

	d := deriveAgentDescription(conf, "generated-id", nil)
	assert.Equal(t, "generated-id", identifyingValue(t, d, keyServiceInstanceID))
}

func TestDeriveAgentDescriptionConfigInstanceIDWins(t *testing.T) {
	conf := resourceConfig(map[string]string{
		keyServiceName:       "svc",
		keyServiceInstanceID: "config-id",
	})

	d := deriveAgentDescription(conf, "generated-id", nil)
	assert.Equal(t, "config-id", identifyingValue(t, d, keyServiceInstanceID))
}

func TestDeriveAgentDescriptionCallerAttrsAreNonIdentifying(t *testing.T) {
	conf := resourceConfig(map[string]string{keyServiceName: "derived"})
	extra := &protobufs.AgentDescription{
		// A caller trying to override identity must not win.
		IdentifyingAttributes: []*protobufs.KeyValue{{Key: keyServiceName, Value: stringValue("caller")}},
		// A genuine supplemental attribute should land in non-identifying.
		NonIdentifyingAttributes: []*protobufs.KeyValue{{Key: "team", Value: stringValue("platform")}},
	}

	d := deriveAgentDescription(conf, "id", extra)

	assert.Equal(t, "derived", identifyingValue(t, d, keyServiceName), "derived identity must win")
	team, ok := findKV(d.GetNonIdentifyingAttributes(), "team")
	require.True(t, ok)
	assert.Equal(t, "platform", team.GetValue().GetStringValue())
}

// fakeClient is a no-op OpAMPClient that records the calls the manager makes.
type fakeClient struct {
	mu           sync.Mutex
	started      bool
	capabilities *protobufs.AgentCapabilities
	descriptions []*protobufs.AgentDescription
	health       *protobufs.ComponentHealth
}

func (c *fakeClient) Start(context.Context, types.StartSettings) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = true
	return nil
}
func (c *fakeClient) Stop(context.Context) error { return nil }
func (c *fakeClient) SetAgentDescription(d *protobufs.AgentDescription) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.descriptions = append(c.descriptions, d)
	return nil
}
func (c *fakeClient) AgentDescription() *protobufs.AgentDescription {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.descriptions) == 0 {
		return nil
	}
	return c.descriptions[len(c.descriptions)-1]
}
func (c *fakeClient) SetHealth(h *protobufs.ComponentHealth) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health = h
	return nil
}
func (c *fakeClient) UpdateEffectiveConfig(context.Context) error               { return nil }
func (c *fakeClient) SetRemoteConfigStatus(*protobufs.RemoteConfigStatus) error { return nil }
func (c *fakeClient) SetPackageStatuses(*protobufs.PackageStatuses) error       { return nil }
func (c *fakeClient) RequestConnectionSettings(*protobufs.ConnectionSettingsRequest) error {
	return nil
}
func (c *fakeClient) SetCustomCapabilities(*protobufs.CustomCapabilities) error { return nil }
func (c *fakeClient) SetFlags(protobufs.AgentToServerFlags)                     {}
func (c *fakeClient) SendCustomMessage(*protobufs.CustomMessage) (chan struct{}, error) {
	return nil, nil
}
func (c *fakeClient) SetAvailableComponents(*protobufs.AvailableComponents) error { return nil }
func (c *fakeClient) SetCapabilities(caps *protobufs.AgentCapabilities) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capabilities = caps
	return nil
}

func TestStartReportsCapabilitiesHealthAndDescription(t *testing.T) {
	fc := &fakeClient{}
	m := newTestManager(t)
	m.newClientFn = func() client.OpAMPClient { return fc }

	require.NoError(t, m.Start(context.Background()))

	fc.mu.Lock()
	defer fc.mu.Unlock()

	require.True(t, fc.started)

	// Capabilities advertise health and heartbeat alongside config handling.
	require.NotNil(t, fc.capabilities)
	caps := *fc.capabilities
	assert.NotZero(t, caps&protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth)
	assert.NotZero(t, caps&protobufs.AgentCapabilities_AgentCapabilities_ReportsHeartbeat)
	assert.NotZero(t, caps&protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)

	// A healthy status is reported after the client starts.
	require.NotNil(t, fc.health)
	assert.True(t, fc.health.GetHealthy())

	// An initial agent description with a service.instance.id is sent.
	require.NotEmpty(t, fc.descriptions)
	assert.NotEmpty(t, identifyingValue(t, fc.descriptions[0], keyServiceInstanceID))
}

func TestAgentDescriptionRefreshOnConfigChange(t *testing.T) {
	fc := &fakeClient{}
	m := newTestManager(t)
	require.NoError(t, m.resolveIdentity(context.Background()))
	m.client = fc

	cfgA := []byte("file_format: \"0.3\"\nresource:\n  attributes:\n    - name: service.name\n      value: svc-a\n")
	cfgB := []byte("file_format: \"0.3\"\nresource:\n  attributes:\n    - name: service.name\n      value: svc-b\n")

	require.NoError(t, m.applyRemoteConfig(context.Background(), remoteConfig(string(cfgA))))
	require.NoError(t, m.applyRemoteConfig(context.Background(), remoteConfig(string(cfgB))))

	fc.mu.Lock()
	defer fc.mu.Unlock()
	require.GreaterOrEqual(t, len(fc.descriptions), 2)
	last := fc.descriptions[len(fc.descriptions)-1]
	assert.Equal(t, "svc-b", identifyingValue(t, last, keyServiceName))
}

func TestInstanceUIDGeneratedPersistedAndReused(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	store1, err := NewFileStateStore(dir)
	require.NoError(t, err)
	m1 := newTestManager(t, WithStateStore(store1))
	require.NoError(t, m1.resolveIdentity(ctx))
	require.NotEqual(t, types.InstanceUid{}, m1.instanceUID, "a UID must be generated")
	require.NotEmpty(t, m1.serviceInstanceID)

	// A second manager over the same state directory reuses the persisted UID.
	store2, err := NewFileStateStore(dir)
	require.NoError(t, err)
	m2 := newTestManager(t, WithStateStore(store2))
	require.NoError(t, m2.resolveIdentity(ctx))
	assert.Equal(t, m1.instanceUID, m2.instanceUID, "UID must be stable across restarts")
	assert.Equal(t, m1.serviceInstanceID, m2.serviceInstanceID)
}

func TestInstanceUIDFromOption(t *testing.T) {
	uid := types.InstanceUid{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10}
	m := newTestManager(t, WithInstanceUID(uid))
	require.NoError(t, m.resolveIdentity(context.Background()))
	assert.Equal(t, uid, m.instanceUID)
}
