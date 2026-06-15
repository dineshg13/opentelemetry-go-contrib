// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp // import "go.opentelemetry.io/contrib/otelconf/opamp"

import (
	"context"
	"sync"

	"github.com/open-telemetry/opamp-go/protobufs"
)

// StateStore persists the OpAMP state the [Manager] must remember across
// restarts: the last reported remote config status and the effective
// configuration. The interface is intentionally small so it can be backed by
// memory, disk, or any other store. Implementations must be safe for concurrent
// use.
type StateStore interface {
	// RemoteConfigStatus returns the last saved remote config status, or nil if
	// none has been saved.
	RemoteConfigStatus(context.Context) (*protobufs.RemoteConfigStatus, error)
	// SaveRemoteConfigStatus saves the remote config status.
	SaveRemoteConfigStatus(context.Context, *protobufs.RemoteConfigStatus) error
	// EffectiveConfig returns the last saved effective configuration bytes, or
	// nil if none has been saved.
	EffectiveConfig(context.Context) ([]byte, error)
	// SaveEffectiveConfig saves the effective configuration bytes.
	SaveEffectiveConfig(context.Context, []byte) error
}

// memoryStateStore is an in-memory, mutex-guarded StateStore. It is sufficient
// for a proof of concept but loses state when the process exits.
type memoryStateStore struct {
	mu              sync.Mutex
	status          *protobufs.RemoteConfigStatus
	effectiveConfig []byte
}

// NewMemoryStateStore returns a StateStore that keeps state in memory.
func NewMemoryStateStore() StateStore {
	return &memoryStateStore{}
}

func (s *memoryStateStore) RemoteConfigStatus(context.Context) (*protobufs.RemoteConfigStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, nil
}

func (s *memoryStateStore) SaveRemoteConfigStatus(_ context.Context, status *protobufs.RemoteConfigStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	return nil
}

func (s *memoryStateStore) EffectiveConfig(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effectiveConfig, nil
}

func (s *memoryStateStore) SaveEffectiveConfig(_ context.Context, cfg []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.effectiveConfig = cfg
	return nil
}
