// SPDX-License-Identifier: AGPL-3.0-or-later

package secret

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestMemoryStore(t *testing.T) {
	m := Memory{}
	if _, err := m.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := m.Set("a", "pw"); err != nil {
		t.Fatal(err)
	}
	if pw, err := m.Get("a"); err != nil || pw != "pw" {
		t.Fatalf("Get = %q, %v", pw, err)
	}
	if err := m.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("a"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestPasswordFromStore(t *testing.T) {
	m := Memory{"a": "s3cret"}
	if pw, err := Password(t.Context(), m, "a", ""); err != nil || pw != "s3cret" {
		t.Fatalf("Password = %q, %v", pw, err)
	}
	if _, err := Password(t.Context(), m, "b", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestPasswordCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	tests := []struct {
		command string
		want    string
		errHas  string
	}{
		{"printf 'hunter2\\nsecond line\\n'", "hunter2", ""},
		{"printf 'crlf\\r\\n'", "crlf", ""},
		{"printf 'no newline'", "no newline", ""},
		{"echo leaked-secret >&2; exit 3", "", "exit status 3"},
		{"true", "", "printed nothing"},
	}
	for _, tt := range tests {
		pw, err := Password(t.Context(), Memory{}, "acct", tt.command)
		if tt.errHas != "" {
			if err == nil || !strings.Contains(err.Error(), tt.errHas) {
				t.Errorf("%q: err = %v, want %q", tt.command, err, tt.errHas)
			}
			if err != nil && strings.Contains(err.Error(), "leaked-secret") {
				t.Errorf("%q: error leaks command output: %v", tt.command, err)
			}
			continue
		}
		if err != nil || pw != tt.want {
			t.Errorf("%q = %q, %v; want %q", tt.command, pw, err, tt.want)
		}
	}
}

func TestPasswordCommandCancelled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Password(ctx, Memory{}, "a", "sleep 5"); err == nil {
		t.Fatal("cancelled command succeeded")
	}
}

func TestLimitedWriter(t *testing.T) {
	var b strings.Builder
	w := &limitedWriter{w: &b, n: 4}
	for _, s := range []string{"ab", "cdef", "gh"} {
		if n, err := w.Write([]byte(s)); err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if b.String() != "abcd" {
		t.Errorf("kept %q", b.String())
	}
}

func TestOSKeyringWithMock(t *testing.T) {
	keyring.MockInit()
	k := Keyring()
	if _, err := k.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v", err)
	}
	if err := k.Set("a", "pw"); err != nil {
		t.Fatal(err)
	}
	if pw, err := k.Get("a"); err != nil || pw != "pw" {
		t.Fatalf("Get = %q, %v", pw, err)
	}
	if err := k.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if err := k.Delete("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing = %v", err)
	}
	keyring.MockInitWithError(errors.New("no dbus"))
	if _, err := k.Get("a"); err == nil || !strings.Contains(err.Error(), "password_command") {
		t.Errorf("Get with broken keyring = %v", err)
	}
	if err := k.Set("a", "pw"); err == nil || !strings.Contains(err.Error(), "--password-command") {
		t.Errorf("Set with broken keyring = %v", err)
	}
}
