// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileStateStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	store, err := NewFileStateStore(dir)
	require.NoError(t, err)

	// Empty store reports no state.
	status, err := store.RemoteConfigStatus(ctx)
	require.NoError(t, err)
	assert.Nil(t, status)
	eff, err := store.EffectiveConfig(ctx)
	require.NoError(t, err)
	assert.Nil(t, eff)

	// Save state.
	want := &protobufs.RemoteConfigStatus{
		LastRemoteConfigHash: []byte("hash-1"),
		Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
	}
	require.NoError(t, store.SaveRemoteConfigStatus(ctx, want))
	require.NoError(t, store.SaveEffectiveConfig(ctx, []byte("file_format: \"0.3\"\n")))

	// A fresh store over the same directory recovers the persisted state,
	// simulating a process restart.
	reopened, err := NewFileStateStore(dir)
	require.NoError(t, err)

	got, err := reopened.RemoteConfigStatus(ctx)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want.GetLastRemoteConfigHash(), got.GetLastRemoteConfigHash())
	assert.Equal(t, want.GetStatus(), got.GetStatus())

	eff, err = reopened.EffectiveConfig(ctx)
	require.NoError(t, err)
	assert.Equal(t, "file_format: \"0.3\"\n", string(eff))
}

func TestFileStateStoreAtomicNoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	store, err := NewFileStateStore(dir)
	require.NoError(t, err)
	require.NoError(t, store.SaveEffectiveConfig(ctx, []byte("data")))
	require.NoError(t, store.SaveRemoteConfigStatus(ctx, &protobufs.RemoteConfigStatus{
		LastRemoteConfigHash: []byte("h"),
	}))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp-", "atomic write left a temporary file behind")
	}
	assert.FileExists(t, filepath.Join(dir, statusFileName))
	assert.FileExists(t, filepath.Join(dir, effectiveFileName))
}

func TestFileStateStoreNilClears(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	store, err := NewFileStateStore(dir)
	require.NoError(t, err)
	require.NoError(t, store.SaveEffectiveConfig(ctx, []byte("data")))
	require.NoError(t, store.SaveEffectiveConfig(ctx, nil))

	eff, err := store.EffectiveConfig(ctx)
	require.NoError(t, err)
	assert.Nil(t, eff)
	assert.NoFileExists(t, filepath.Join(dir, effectiveFileName))
}

func TestNewFileStateStoreRequiresDir(t *testing.T) {
	_, err := NewFileStateStore("")
	assert.Error(t, err)
}
