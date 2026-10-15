// SPDX-License-Identifier: AGPL-3.0-or-later

// Package lock provides an exclusive, non-blocking, process-wide file lock.
// The OS releases it when the process exits, so a crash never leaves a
// stale lock behind.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked means another process holds the lock.
var ErrLocked = errors.New("locked by another process")

// Lock is a held lock.
type Lock struct {
	f *os.File
}

// Acquire takes the lock at path, failing immediately with ErrLocked if
// another process holds it.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return &Lock{f: f}, nil
}

// Release releases the lock.
func (l *Lock) Release() error {
	return errors.Join(unlockFile(l.f), l.f.Close())
}
