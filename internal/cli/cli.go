// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cli is the command-line frontend. Each command is a thin adapter:
// it binds flags to an op's input, calls the op, and renders the output as
// text or JSON. No business logic lives here.
package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/spf13/cobra"

	"nought/internal/autoconfig"
	"nought/internal/config"
	"nought/internal/ops"
	"nought/internal/secret"
)

// Options is everything Run needs from the outside world. Injecting it keeps
// the CLI testable without touching the real process environment.
type Options struct {
	Args    []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getenv  func(string) string
	Version string
	Secrets secret.Store // nil means the OS keyring
	TLS     *tls.Config  // base TLS config for mail servers; nil means system roots
	// Interactive overrides terminal detection for prompts (tests only).
	Interactive *bool
}

// app is the state shared by all commands during one invocation.
type app struct {
	opts    Options
	json    bool
	verbose bool
	env     *ops.Env
}

// The CLI acts for the local user, who holds every permission.
const callerPerm = ops.Delete

// Run executes one command line and returns the process exit code.
func Run(ctx context.Context, o Options) int {
	a := &app{opts: o}
	root := newRootCmd(a)
	root.SetArgs(o.Args)
	root.SetIn(o.Stdin)
	root.SetOut(o.Stdout)
	root.SetErr(o.Stderr)

	err := root.ExecuteContext(ctx)
	if a.env != nil {
		err = errors.Join(err, a.env.Close())
	}
	if err == nil {
		return exitOK
	}
	code, name := exitCode(err)
	writeError(o.Stderr, a.json || wantsJSON(o.Args), name, err)
	return code
}

// wantsJSON detects --json when flag parsing failed before setting a.json.
func wantsJSON(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--":
			return false
		case "--json", "--json=true", "--json=1":
			return true
		}
	}
	return false
}

// noPaths marks commands that work without resolvable data paths.
const noPaths = "nopaths"

// setup builds the Env once flags are parsed.
func (a *app) setup(cmd *cobra.Command, _ []string) error {
	tlsConfig, err := a.devMode()
	if err != nil {
		return err
	}
	if tlsConfig != nil {
		a.opts.TLS = tlsConfig
	}
	paths, err := config.ResolvePaths(runtime.GOOS, a.opts.Getenv)
	if err != nil && cmd.Annotations[noPaths] == "" {
		return err
	}
	level := slog.LevelWarn
	if a.verbose || a.opts.Getenv("NOU_LOG") == "debug" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(a.opts.Stderr, &slog.HandlerOptions{Level: level}))
	secrets := a.opts.Secrets
	if secrets == nil {
		secrets = secret.Keyring()
	}
	a.env = ops.NewEnv(paths, log, time.Now, buildInfo(a.opts.Version), secrets)
	a.env.TLS = a.opts.TLS
	a.env.Network = autoconfig.DefaultNetwork()
	return nil
}

// devMode applies NOU_DEV=1, used while developing nought: it refuses to run
// without NOUGHT_HOME, so a development run can never touch the user's real
// mail, and it trusts the test server's certificate from NOU_DEV_CA (in
// addition to the system roots).
func (a *app) devMode() (*tls.Config, error) {
	if a.opts.Getenv("NOU_DEV") != "1" {
		return nil, nil
	}
	if a.opts.Getenv("NOUGHT_HOME") == "" {
		return nil, fmt.Errorf("NOU_DEV=1 requires NOUGHT_HOME (development must never use your real data): %w", ops.ErrUsage)
	}
	caFile := a.opts.Getenv("NOU_DEV_CA")
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("NOU_DEV_CA: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("NOU_DEV_CA: no certificate in %s: %w", caFile, ops.ErrUsage)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

func buildInfo(version string) ops.BuildInfo {
	b := ops.BuildInfo{Version: version, GoVersion: runtime.Version()}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				b.Commit = s.Value
			}
		}
		if b.Version == "" || b.Version == "dev" {
			if v := info.Main.Version; v != "" && v != "(devel)" {
				b.Version = v
			}
		}
	}
	if b.Version == "" {
		b.Version = "dev"
	}
	return b
}

// interactive reports whether nou may prompt: only when both stdin and
// stdout are terminals, never in scripts or for agents.
func (a *app) interactive() bool {
	if a.opts.Interactive != nil {
		return *a.opts.Interactive
	}
	in, ok := a.opts.Stdin.(*os.File)
	return ok && isTerminal(in) && isTerminal(a.opts.Stdout)
}
