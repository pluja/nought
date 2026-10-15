// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"nought/internal/ops"
	"nought/internal/secret"
)

var update = flag.Bool("update", false, "update testscript golden output")

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"nou": func() {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			code := Run(ctx, Options{
				Args: os.Args[1:], Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
				Getenv: os.Getenv, Version: "test", Secrets: fileSecrets(os.Getenv("NOU_TEST_KEYRING")),
			})
			stop()
			os.Exit(code)
		},
	})
}

func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:           filepath.Join("testdata", "script"),
		UpdateScripts: *update,
		Cmds:          map[string]func(*testscript.TestScript, bool, []string){"code": cmdCode},
		Setup: func(e *testscript.Env) error {
			e.Setenv("NOUGHT_HOME", filepath.Join(e.WorkDir, "home"))
			e.Setenv("NOU_TEST_KEYRING", filepath.Join(e.WorkDir, "keyring.json"))
			return nil
		},
	})
}

// cmdCode runs a command and checks its exit status:
//
//	code 2 nou bogus
func cmdCode(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) < 2 {
		ts.Fatalf("usage: code <status> <command> [args...]")
	}
	want, err := strconv.Atoi(args[0])
	if err != nil {
		ts.Fatalf("bad status %q", args[0])
	}
	got := 0
	if err := ts.Exec(args[1], args[2:]...); err != nil {
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			ts.Fatalf("run %s: %v", args[1], err)
		}
		got = exitErr.ExitCode()
	}
	if got != want {
		ts.Fatalf("exit status %d, want %d", got, want)
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		err      error
		wantCode int
		wantName string
	}{
		{fmt.Errorf("x: %w", ops.ErrUsage), exitUsage, "usage"},
		{fmt.Errorf("x: %w", ops.ErrPartial), exitPartial, "partial"},
		{fmt.Errorf("x: %w", ops.ErrPermission), exitPermission, "permission"},
		{fmt.Errorf("x: %w", ops.ErrAmbiguous), exitNotFound, "ambiguous"},
		{fmt.Errorf("x: %w", ops.ErrNotFound), exitNotFound, "not_found"},
		{errors.New("boom"), exitInternal, "internal"},
	}
	for _, tt := range tests {
		code, name := exitCode(tt.err)
		if code != tt.wantCode || name != tt.wantName {
			t.Errorf("exitCode(%v) = %d, %q; want %d, %q", tt.err, code, name, tt.wantCode, tt.wantName)
		}
	}
}

func TestBuildInfoDefaultsToDev(t *testing.T) {
	if got := buildInfo("").Version; got == "" {
		t.Error("empty version")
	}
	if got := buildInfo("v1.2.3").Version; got != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", got)
	}
}

// fileSecrets is a test keyring persisted in a JSON file, so state survives
// across the separate nou processes of a test script.
type fileSecrets string

func (f fileSecrets) load() map[string]string {
	m := map[string]string{}
	if data, err := os.ReadFile(string(f)); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

func (f fileSecrets) save(m map[string]string) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(string(f), data, 0o600)
}

func (f fileSecrets) Get(account string) (string, error) {
	pw, ok := f.load()[account]
	if !ok {
		return "", secret.ErrNotFound
	}
	return pw, nil
}

func (f fileSecrets) Set(account, password string) error {
	m := f.load()
	m[account] = password
	return f.save(m)
}

func (f fileSecrets) Delete(account string) error {
	m := f.load()
	if _, ok := m[account]; !ok {
		return secret.ErrNotFound
	}
	delete(m, account)
	return f.save(m)
}
