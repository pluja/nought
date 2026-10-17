// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Failure paths that need Unix permissions to provoke. Each must leave the
// archive as it was: never a half-written message, never a lost one.

func readOnly(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

func openStore(t *testing.T) (*Maildir, string) {
	t.Helper()
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, dir
}

func TestPutFailsCleanlyWhenTmpIsReadOnly(t *testing.T) {
	m, dir := openStore(t)
	readOnly(t, filepath.Join(dir, "tmp"))
	raw := []byte("Subject: x\r\n\r\nx\r\n")
	if _, _, err := m.Put(raw, ""); err == nil {
		t.Fatal("Put succeeded without a writable tmp/")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "cur"))
	if m.Has(Hash(raw)) || len(entries) != 0 {
		t.Errorf("a failed Put left a message: has %v, cur %d", m.Has(Hash(raw)), len(entries))
	}
}

func TestMoveKeepsTheSourceWhenTheDestinationFails(t *testing.T) {
	src, _ := openStore(t)
	dst, dstDir := openStore(t)
	raw := []byte("Subject: keep\r\n\r\nkeep\r\n")
	e, _, err := src.Put(raw, "S")
	if err != nil {
		t.Fatal(err)
	}
	readOnly(t, filepath.Join(dstDir, "tmp"))
	if err := src.MoveTo(dst, e.Hash); err == nil {
		t.Fatal("MoveTo succeeded into a read-only destination")
	}
	if got, err := src.GetVerified(e.Hash); err != nil || string(got) != string(raw) {
		t.Errorf("source lost the message: %q, %v", got, err)
	}
}

func TestFailedRemoveKeepsTheMessage(t *testing.T) {
	m, dir := openStore(t)
	raw := []byte("Subject: stay\r\n\r\nstay\r\n")
	e, _, err := m.Put(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	readOnly(t, filepath.Join(dir, "cur"))
	if err := m.Remove(e.Hash); err == nil {
		t.Fatal("Remove succeeded in a read-only cur/")
	}
	if got, err := m.GetVerified(e.Hash); err != nil || string(got) != string(raw) {
		t.Errorf("message lost: %q, %v", got, err)
	}
}

// A file that cannot be read is an error, not "not found": callers treat
// not found as gone, and act on it.
func TestUnreadableIsNotNotFound(t *testing.T) {
	m, dir := openStore(t)
	e, _, err := m.Put([]byte("Subject: x\r\n\r\nx\r\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	name, err := m.lookup(e.Hash)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	if _, err := m.Get(e.Hash); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("unreadable file: %v", err)
	}
}
