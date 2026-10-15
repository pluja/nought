// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secret finds account passwords: from a user-configured command
// (like mbsync's PassCmd), or from the OS keyring (Secret Service on Linux,
// Keychain on macOS, Credential Manager on Windows). Passwords are never
// written to config files or logs, and never appear in error messages.
package secret

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

// ErrNotFound means no password is stored for the account.
var ErrNotFound = errors.New("no password stored")

// Store keeps passwords per account.
type Store interface {
	Get(account string) (string, error)
	Set(account, password string) error
	Delete(account string) error
}

// service names nought's entries in the OS keyring.
const service = "nought"

// Keyring returns the OS keyring.
func Keyring() Store { return osKeyring{} }

type osKeyring struct{}

func (osKeyring) Get(account string) (string, error) {
	pw, err := keyring.Get(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read OS keyring (on a headless system, set password_command instead): %w", err)
	}
	return pw, nil
}

func (osKeyring) Set(account, password string) error {
	if err := keyring.Set(service, account, password); err != nil {
		return fmt.Errorf("write OS keyring (on a headless system, use --password-command instead): %w", err)
	}
	return nil
}

func (osKeyring) Delete(account string) error {
	err := keyring.Delete(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// Memory is an in-memory Store for tests.
type Memory map[string]string

// Get implements Store.
func (m Memory) Get(account string) (string, error) {
	pw, ok := m[account]
	if !ok {
		return "", ErrNotFound
	}
	return pw, nil
}

// Set implements Store.
func (m Memory) Set(account, password string) error { m[account] = password; return nil }

// Delete implements Store.
func (m Memory) Delete(account string) error {
	if _, ok := m[account]; !ok {
		return ErrNotFound
	}
	delete(m, account)
	return nil
}

// Limits for password commands.
const (
	commandTimeout   = 60 * time.Second // allows for an interactive unlock (e.g. a GPG agent prompt)
	waitDelay        = 2 * time.Second
	maxCommandOutput = 64 << 10
)

// Password returns the account's password: the first line printed by
// command if one is configured, otherwise the keyring entry.
func Password(ctx context.Context, s Store, account, command string) (string, error) {
	if command == "" {
		pw, err := s.Get(account)
		if err != nil {
			return "", fmt.Errorf("password for %s: %w", account, err)
		}
		return pw, nil
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := shellCommand(ctx, command)
	var stdout bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &stdout, n: maxCommandOutput}
	// stderr goes nowhere: it could echo the secret. Exit status is enough.
	// A background child that keeps stdout open must not block us: the
	// whole process group is killed on timeout, and output copying stops
	// shortly after the command itself exits.
	cmd.WaitDelay = waitDelay
	setProcessGroup(cmd)
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return "", fmt.Errorf("password_command for %s failed: %s", account, exitReason(ctx, err))
	}
	line, err := bufio.NewReader(&stdout).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	pw := strings.TrimRight(line, "\r\n")
	if pw == "" {
		return "", fmt.Errorf("password_command for %s printed nothing", account)
	}
	return pw, nil
}

// shellCommand runs the user's own password_command from their config file,
// through the shell, as mbsync's PassCmd does. Running it is the point.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command) //nolint:gosec // G204: user-configured command
	}
	return exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // G204: user-configured command
}

// exitReason describes a failure without including any command output.
func exitReason(ctx context.Context, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timed out"
	case ctx.Err() != nil:
		return "cancelled"
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return fmt.Sprintf("exit status %d", exitErr.ExitCode())
	}
	return "could not run it"
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil // discard the rest; only the first line matters
	}
	keep := p[:min(len(p), l.n)]
	l.n -= len(keep)
	if _, err := l.w.Write(keep); err != nil {
		return 0, err
	}
	return len(p), nil
}
