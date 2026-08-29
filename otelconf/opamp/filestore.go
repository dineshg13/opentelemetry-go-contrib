// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package opamp // import "go.opentelemetry.io/contrib/otelconf/opamp"

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// File names used within the state directory.
const (
	instanceUIDFileName = "instance_uid"
	statusFileName      = "remote_config_status.pb"
	effectiveFileName   = "effective_config.yaml"
)

// fileStateStore is a StateStore backed by files in a directory: the agent
// instance UID (raw 16 bytes), the remote config status (protobuf wire format),
// and the effective configuration (the raw configuration bytes). Writes are
// atomic — data is written to a temporary file, flushed, and renamed into place
// — so a crash mid-write cannot leave a torn or partially written file. All
// access is serialized by a mutex.
type fileStateStore struct {
	mu              sync.Mutex
	instanceUIDPath string
	statusPath      string
	effectivePath   string
}

// NewFileStateStore returns a StateStore that persists state under dir, creating
// the directory if needed. It is suitable for real applications: state survives
// restarts, so on reconnect the agent reports its last applied configuration and
// status rather than starting from scratch.
func NewFileStateStore(dir string) (StateStore, error) {
	if dir == "" {
		return nil, errors.New("opamp: state directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("opamp: create state directory: %w", err)
	}
	return &fileStateStore{
		instanceUIDPath: filepath.Join(dir, instanceUIDFileName),
		statusPath:      filepath.Join(dir, statusFileName),
		effectivePath:   filepath.Join(dir, effectiveFileName),
	}, nil
}

func (s *fileStateStore) InstanceUID(context.Context) (types.InstanceUid, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := os.ReadFile(s.instanceUIDPath)
	if errors.Is(err, fs.ErrNotExist) {
		return types.InstanceUid{}, false, nil
	}
	if err != nil {
		return types.InstanceUid{}, false, fmt.Errorf("opamp: read instance uid: %w", err)
	}
	if len(b) != len(types.InstanceUid{}) {
		return types.InstanceUid{}, false, fmt.Errorf("opamp: invalid instance uid length %d", len(b))
	}
	var uid types.InstanceUid
	copy(uid[:], b)
	return uid, true, nil
}

func (s *fileStateStore) SaveInstanceUID(_ context.Context, uid types.InstanceUid) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeFileAtomic(s.instanceUIDPath, uid[:], 0o600)
}

func (s *fileStateStore) RemoteConfigStatus(context.Context) (*protobufs.RemoteConfigStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := os.ReadFile(s.statusPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opamp: read remote config status: %w", err)
	}
	status := &protobufs.RemoteConfigStatus{}
	if err := proto.Unmarshal(b, status); err != nil {
		return nil, fmt.Errorf("opamp: decode remote config status: %w", err)
	}
	return status, nil
}

func (s *fileStateStore) SaveRemoteConfigStatus(_ context.Context, status *protobufs.RemoteConfigStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// A nil status clears any persisted state.
	if status == nil {
		return removeIfExists(s.statusPath)
	}
	b, err := proto.Marshal(status)
	if err != nil {
		return fmt.Errorf("opamp: encode remote config status: %w", err)
	}
	return writeFileAtomic(s.statusPath, b, 0o600)
}

func (s *fileStateStore) EffectiveConfig(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := os.ReadFile(s.effectivePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opamp: read effective config: %w", err)
	}
	return b, nil
}

func (s *fileStateStore) SaveEffectiveConfig(_ context.Context, cfg []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cfg == nil {
		return removeIfExists(s.effectivePath)
	}
	return writeFileAtomic(s.effectivePath, cfg, 0o600)
}

// removeIfExists deletes path, treating a missing file as success.
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("opamp: remove %s: %w", filepath.Base(path), err)
	}
	return nil
}

// writeFileAtomic writes data to path atomically: it writes to a temporary file
// in the same directory, flushes it to stable storage, and renames it over path.
// The parent directory is flushed so the rename itself survives a crash. A
// failure before the rename leaves path unchanged.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("opamp: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup of the temp file on any error path; after a successful
	// rename it no longer exists and Remove is a harmless no-op.
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("opamp: write temp file: %w", err)
	}
	if err = tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("opamp: chmod temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("opamp: sync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("opamp: close temp file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("opamp: rename temp file: %w", err)
	}

	// Flush the directory entry so the rename is durable. Best effort: not all
	// platforms support syncing a directory.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
