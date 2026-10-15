// SPDX-License-Identifier: AGPL-3.0-or-later

package lock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "sync.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	// flock locks belong to the open file description, so a second
	// Acquire in the same process conflicts just as another process would.
	if _, err := Acquire(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second Acquire = %v, want ErrLocked", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after Release = %v", err)
	}
	_ = again.Release()
}

func TestAcquireBadPath(t *testing.T) {
	if _, err := Acquire(filepath.Join(t.TempDir(), "x\x00y")); err == nil {
		t.Fatal("expected an error")
	}
}
