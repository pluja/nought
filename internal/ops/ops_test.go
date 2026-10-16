// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/secret"
	"github.com/pluja/nought/internal/store"
)

// newTestEnv returns an Env rooted in a temporary directory.
func newTestEnv(t *testing.T) *Env {
	t.Helper()
	home := t.TempDir()
	paths, err := config.ResolvePaths("linux", func(k string) string {
		if k == "NOUGHT_HOME" {
			return home
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	env := NewEnv(paths, slog.New(slog.DiscardHandler), func() time.Time { return now },
		BuildInfo{Version: "test", GoVersion: "go1.26"}, secret.Memory{})
	t.Cleanup(func() {
		if err := env.Close(); err != nil {
			t.Errorf("close env: %v", err)
		}
	})
	return env
}

func TestCallChecksPermission(t *testing.T) {
	env := newTestEnv(t)
	ran := false
	op := Op[int, int]{Name: "test.op", Perm: Tag, Run: func(_ context.Context, _ *Env, in int) (int, error) {
		ran = true
		return in * 2, nil
	}}

	if _, err := Call(t.Context(), env, Read, op, 1); !errors.Is(err, ErrPermission) {
		t.Fatalf("err = %v, want ErrPermission", err)
	}
	if ran {
		t.Fatal("op ran without permission")
	}

	got, err := Call(t.Context(), env, Delete, op, 21)
	if err != nil || got != 42 {
		t.Fatalf("Call = %d, %v; want 42, nil", got, err)
	}
}

func TestPermissionString(t *testing.T) {
	for p, want := range map[Permission]string{Read: "read", Tag: "tag", Draft: "draft", Send: "send", Delete: "delete", 99: "permission(99)"} {
		if got := p.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(p), got, want)
		}
	}
}

func TestUsageErrorWraps(t *testing.T) {
	err := usageError("bad flag %q", "x")
	if !errors.Is(err, ErrUsage) || err.Error() != `bad flag "x": usage error` {
		t.Fatalf("usageError = %v", err)
	}
}

func TestPathsShow(t *testing.T) {
	env := newTestEnv(t)
	out, err := Call(t.Context(), env, Read, PathsShow, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if out.IndexFile != filepath.Join(env.Paths.DataDir, "index.db") {
		t.Errorf("IndexFile = %q", out.IndexFile)
	}
	if len(out.Backup.Include) == 0 || out.Backup.Optional[0] != out.IndexFile {
		t.Errorf("backup = %+v", out.Backup)
	}
}

func TestVersionShow(t *testing.T) {
	env := newTestEnv(t)
	out, err := Call(t.Context(), env, Read, VersionShow, struct{}{})
	if err != nil || out.Version != "test" {
		t.Fatalf("VersionShow = %+v, %v", out, err)
	}
}

func TestEnvConfigLoadsOnce(t *testing.T) {
	env := newTestEnv(t)
	c1, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	c2, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Error("config loaded twice")
	}
}

// removeFile deletes a stored message file, simulating loss or tampering.
func removeFile(env *Env, account string, e store.Entry) error {
	matches, err := filepath.Glob(filepath.Join(env.Paths.AccountArchive(account), "cur", e.Hash+"*"))
	if err != nil || len(matches) != 1 {
		return fmt.Errorf("find %s: %v: %w", e.Hash, matches, err)
	}
	return os.Remove(matches[0])
}
