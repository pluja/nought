// SPDX-License-Identifier: AGPL-3.0-or-later

package atomicfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteBytesCreatesAndReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "file.txt")

	for _, content := range []string{"first", "second"} {
		if err := WriteBytes(path, 0o600, []byte(content)); err != nil {
			t.Fatalf("WriteBytes(%q): %v", content, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != content {
			t.Errorf("content = %q, want %q", got, content)
		}
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("perm = %o, want 600", perm)
		}
	}
	assertNoTempFiles(t, filepath.Dir(path))
}

func TestWriteFillErrorKeepsOldContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := WriteBytes(path, 0o600, []byte("old")); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("boom")
	err := Write(path, 0o600, func(w io.Writer) error {
		_, _ = w.Write([]byte("partial"))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Errorf("content = %q, want old content preserved", got)
	}
	assertNoTempFiles(t, filepath.Dir(path))
}

func TestSyncDirMissing(t *testing.T) {
	if err := SyncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("SyncDir on a missing directory succeeded")
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if matched, _ := filepath.Match(".*.tmp-*", e.Name()); matched {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}
