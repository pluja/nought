// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const msg = "From: a@example.org\r\nSubject: hi\r\n\r\nbody\r\n"

func open(t *testing.T, dir string) *Maildir {
	t.Helper()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

func TestPutGetRoundTrip(t *testing.T) {
	m := open(t, t.TempDir())
	e, created, err := m.Put([]byte(msg), "SF")
	if err != nil || !created {
		t.Fatalf("Put = %v, %v", created, err)
	}
	if e.Hash != Hash([]byte(msg)) || e.Flags != "FS" {
		t.Errorf("entry = %+v", e)
	}
	got, err := m.Get(e.Hash)
	if err != nil || string(got) != msg {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if !m.Has(e.Hash) || m.Len() != 1 {
		t.Error("Has/Len wrong after Put")
	}
}

func TestPutIsIdempotent(t *testing.T) {
	m := open(t, t.TempDir())
	first, _, err := m.Put([]byte(msg), "S")
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := m.Put([]byte(msg), "F")
	if err != nil || created {
		t.Fatalf("second Put = %v, %v; want existing", created, err)
	}
	if second != first {
		t.Errorf("second entry %+v, want %+v (existing flags kept)", second, first)
	}
	if m.Len() != 1 {
		t.Errorf("Len = %d", m.Len())
	}
}

func TestPutRejectsBadFlags(t *testing.T) {
	m := open(t, t.TempDir())
	if _, _, err := m.Put([]byte(msg), "X"); err == nil {
		t.Fatal("accepted flag X")
	}
}

func TestReopenFindsMessages(t *testing.T) {
	dir := t.TempDir()
	m := open(t, dir)
	e, _, err := m.Put([]byte(msg), "S")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cur", "foreign-file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	again := open(t, dir)
	if !again.Has(e.Hash) || again.Foreign() != 1 {
		t.Errorf("Has = %v, Foreign = %d", again.Has(e.Hash), again.Foreign())
	}
	flags, err := again.Flags(e.Hash)
	if err != nil || flags != "S" {
		t.Errorf("Flags = %q, %v", flags, err)
	}
}

func TestSetFlags(t *testing.T) {
	dir := t.TempDir()
	m := open(t, dir)
	e, _, err := m.Put([]byte(msg), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetFlags(e.Hash, "SRS"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetFlags(e.Hash, "RS"); err != nil { // no-op
		t.Fatal(err)
	}
	if flags, _ := m.Flags(e.Hash); flags != "RS" {
		t.Errorf("flags = %q", flags)
	}
	if _, err := os.Stat(filepath.Join(dir, "cur", fileName(e.Hash, "RS"))); err != nil {
		t.Errorf("renamed file missing: %v", err)
	}
	if err := m.Verify(e.Hash); err != nil {
		t.Errorf("Verify after rename: %v", err)
	}
	if err := m.SetFlags(e.Hash, "Z"); err == nil {
		t.Error("accepted flag Z")
	}
}

func TestMissingMessage(t *testing.T) {
	m := open(t, t.TempDir())
	missing := strings.Repeat("0", 64)
	if _, err := m.Get(missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get: %v", err)
	}
	if _, err := m.Flags(missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("Flags: %v", err)
	}
	if err := m.SetFlags(missing, "S"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetFlags: %v", err)
	}
	if err := m.Verify(missing); !errors.Is(err, ErrNotFound) {
		t.Errorf("Verify: %v", err)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	dir := t.TempDir()
	m := open(t, dir)
	e, _, err := m.Put([]byte(msg), "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cur", fileName(e.Hash, ""))
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(e.Hash); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Verify = %v, want ErrCorrupt", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := m.Verify(e.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("Verify of deleted file = %v, want ErrNotFound", err)
	}
}

// A crash at any step before the rename must leave no visible message; a
// crash after the rename leaves a complete message that a reopen finds.
func TestCrashAtEachStep(t *testing.T) {
	crash := errors.New("crash")
	tests := []struct {
		name        string
		set         func(*hooks)
		visibleNext bool
	}{
		{"after write", func(h *hooks) { h.afterWrite = func() error { return crash } }, false},
		{"after sync", func(h *hooks) { h.afterSync = func() error { return crash } }, false},
		{"after rename", func(h *hooks) { h.afterRename = func() error { return crash } }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			m := open(t, dir)
			tt.set(&m.hooks)
			if _, _, err := m.Put([]byte(msg), ""); !errors.Is(err, crash) {
				t.Fatalf("Put = %v, want crash", err)
			}
			if m.Has(Hash([]byte(msg))) {
				t.Error("failed Put registered the message")
			}
			assertEmpty(t, filepath.Join(dir, "tmp"))

			again := open(t, dir)
			if got := again.Has(Hash([]byte(msg))); got != tt.visibleNext {
				t.Errorf("visible after reopen = %v, want %v", got, tt.visibleNext)
			}
			if tt.visibleNext {
				if err := again.Verify(Hash([]byte(msg))); err != nil {
					t.Errorf("message after crash is not intact: %v", err)
				}
			}
		})
	}
}

func TestOpenRemovesStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	_ = open(t, dir)
	stale := filepath.Join(dir, "tmp", tempPrefix+"abc")
	other := filepath.Join(dir, "tmp", "someone-else")
	for _, p := range []string{stale, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = open(t, dir)
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Error("stale temp file survived")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("removed another program's temp file")
	}
}

func TestAllIsSorted(t *testing.T) {
	m := open(t, t.TempDir())
	for _, body := range []string{"a", "b", "c", "d"} {
		if _, _, err := m.Put([]byte(body), ""); err != nil {
			t.Fatal(err)
		}
	}
	var hashes []string
	for e := range m.All() {
		hashes = append(hashes, e.Hash)
	}
	if len(hashes) != 4 || !slices.IsSorted(hashes) {
		t.Errorf("All = %v", hashes)
	}
	n := 0
	for range m.All() {
		n++
		break
	}
	if n != 1 {
		t.Error("All ignored early break")
	}
}

func TestPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	dir := filepath.Join(t.TempDir(), "acct")
	m := open(t, dir)
	e, _, err := m.Put([]byte(msg), "")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		dir:                       0o700,
		filepath.Join(dir, "cur"): 0o700,
		filepath.Join(dir, "cur", fileName(e.Hash, "")): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s: perm %o, want %o", path, got, want)
		}
	}
}

func TestParseName(t *testing.T) {
	h := strings.Repeat("ab", 32)
	tests := []struct {
		name      string
		wantFlags string
		wantOK    bool
	}{
		{h + ":2,FS", "FS", true},
		{h + ";2,S", "S", true},
		{h + ":2,", "", true},
		{h + ":2,SF", "FS", true},
		{h + ":1,S", "", false},
		{h + ":2,X", "", false},
		{h, "", false},
		{strings.Repeat("zz", 32) + ":2,", "", false},
		{"short", "", false},
	}
	for _, tt := range tests {
		hash, flags, ok := parseName(tt.name)
		if ok != tt.wantOK || flags != tt.wantFlags || (ok && hash != h) {
			t.Errorf("parseName(%q) = %q, %q, %v", tt.name, hash, flags, ok)
		}
	}
}

func TestOpenFailsOnFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("Open on a regular file succeeded")
	}
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("%s not empty: %v", dir, entries)
	}
}

func FuzzParseName(f *testing.F) {
	f.Add(strings.Repeat("ab", 32) + ":2,FS")
	f.Add("x")
	f.Fuzz(func(t *testing.T, name string) {
		hash, flags, ok := parseName(name)
		if !ok {
			return
		}
		if again, _, ok2 := parseName(fileName(hash, flags)); !ok2 || again != hash {
			t.Fatalf("round trip failed for %q", name)
		}
	})
}
