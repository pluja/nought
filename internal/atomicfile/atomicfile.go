// SPDX-License-Identifier: AGPL-3.0-or-later

// Package atomicfile writes whole files durably: readers see either the old
// content or the new content, never a partial write, even across crashes.
package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Write replaces path with the bytes produced by fill. The data is written to
// a temporary file in the same directory, fsynced, renamed over path, and the
// directory is fsynced so the rename itself is durable.
func Write(path string, perm os.FileMode, fill func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			err = errors.Join(err, removeIfExists(tmp))
		}
	}()

	if err := f.Chmod(perm); err != nil {
		return errors.Join(fmt.Errorf("chmod temp file: %w", err), f.Close())
	}
	if err := fill(f); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync temp file: %w", err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	return SyncDir(dir)
}

// WriteBytes is Write for content that is already in memory.
func WriteBytes(path string, perm os.FileMode, data []byte) error {
	return Write(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// SyncDir fsyncs a directory so that entries created or renamed in it are
// durable. Directory sync is not supported on Windows, where it is a no-op.
func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	if err := syncDir(d); err != nil {
		return errors.Join(fmt.Errorf("sync directory: %w", err), d.Close())
	}
	return d.Close()
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove temp file: %w", err)
	}
	return nil
}
