// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cli is the command-line frontend. Each command is a thin adapter:
// it binds flags to an op's input, calls the op, and renders the output as
// text or JSON. No business logic lives here.
package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/spf13/cobra"

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
	return nil
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
