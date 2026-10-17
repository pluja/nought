// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/ops"
	"github.com/pluja/nought/internal/secret"
	"github.com/pluja/nought/internal/smtptest"
)

// composeApp is an app whose account sends to a test SMTP server, with an
// editor that writes the given versions of the file in turn ("": quit
// without saving).
func composeApp(t *testing.T, stdin string, versions ...string) (*app, *smtptest.Server, *[]string) {
	t.Helper()
	srv := smtptest.Start(t, smtptest.Options{})
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
	env := ops.NewEnv(paths, slog.New(slog.DiscardHandler), time.Now, ops.BuildInfo{}, secret.Memory{})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(srv.CAPEM)
	env.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	t.Cleanup(func() { _ = env.Close() })
	if _, err := ops.Call(t.Context(), env, ops.Delete, ops.AccountAdd, ops.AccountIn{Name: "test", Email: smtptest.User,
		IMAPHost: "127.0.0.1", Password: smtptest.Password, SMTPHost: srv.Host, SMTPPort: srv.Port}); err != nil {
		t.Fatal(err)
	}
	var edited []string
	yes := true
	a := &app{env: env, perm: ops.Delete, opts: Options{Stdin: strings.NewReader(stdin), Interactive: &yes}}
	a.editor = func(_ context.Context, path string) error {
		if len(edited) == len(versions) {
			t.Fatal("the editor opened too often")
		}
		edited = append(edited, path)
		if versions[len(edited)-1] == "" {
			return nil // quit without saving
		}
		return os.WriteFile(path, []byte(versions[len(edited)-1]), 0o600)
	}
	return a, srv, &edited
}

func runCompose(t *testing.T, a *app) (string, error) {
	t.Helper()
	var out strings.Builder
	cmd := newComposeCmd(a)
	cmd.SetContext(t.Context())
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	return out.String(), err
}

func TestComposeInTerminal(t *testing.T) {
	const ready = "From: me@example.org\nTo: alice@example.org\nSubject: Hi\n\nHello\n"
	tests := []struct {
		name     string
		stdin    string
		versions []string
		sent     int
		drafts   int
		want     string
	}{
		{"edit then send", "s\n", []string{ready}, 1, 0, "sent: Hi"},
		{"edit twice, discard", "e\nq\ny\n", []string{ready, ready}, 0, 0, "Discarded."},
		{"discard asks first", "q\nn\nd\n", []string{ready}, 0, 1, "kept"},
		{"refused, kept", "s\nd\n", []string{"From: me@example.org\nSubject: Hi\n\nHello\n"}, 0, 1, "cannot send: no recipients"},
		{"input ends", "", []string{ready}, 0, 1, "kept"},
		{"nothing written", "", []string{""}, 0, 0, "discarded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, srv, edited := composeApp(t, tt.stdin, tt.versions...)
			out, err := runCompose(t, a)
			if err != nil {
				t.Fatalf("compose: %v\n%s", err, out)
			}
			drafts, err := ops.Call(t.Context(), a.env, ops.Delete, ops.DraftList, struct{}{})
			if err != nil {
				t.Fatal(err)
			}
			if len(*edited) != len(tt.versions) || len(srv.Messages()) != tt.sent || len(drafts) != tt.drafts || !strings.Contains(out, tt.want) {
				t.Errorf("edited %d, sent %d, drafts %d, output:\n%s", len(*edited), len(srv.Messages()), len(drafts), out)
			}
		})
	}
}

func TestEditorCommand(t *testing.T) {
	fallback := "vi"
	if runtime.GOOS == "windows" {
		fallback = "notepad"
	}
	tests := []struct {
		visual, editor string
		want           []string
	}{
		{"", "", []string{fallback}},
		{"", "nano", []string{"nano"}},
		{"code --wait", "nano", []string{"code", "--wait"}},
		{"  ", "nano", []string{"nano"}},
	}
	for _, tt := range tests {
		env := map[string]string{"VISUAL": tt.visual, "EDITOR": tt.editor}
		a := &app{opts: Options{Getenv: func(k string) string { return env[k] }}}
		if got := a.editorCommand(); !slices.Equal(got, tt.want) {
			t.Errorf("VISUAL=%q EDITOR=%q: %q, want %q", tt.visual, tt.editor, got, tt.want)
		}
	}
}

func TestPrintSyncShowsTheOutbox(t *testing.T) {
	var b strings.Builder
	out := ops.SyncOut{Outbox: ops.FlushOut{Sent: 1, Failed: 1, Notes: []string{"\"Hi\" may have been sent"}}}
	if err := printSync(&b, out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "outbox: sent 1, failed 1") || !strings.Contains(b.String(), "may have been sent") {
		t.Errorf("output:\n%s", b.String())
	}
	b.Reset()
	if err := printSync(&b, ops.SyncOut{}); err != nil || strings.Contains(b.String(), "outbox") {
		t.Errorf("an idle outbox is mentioned:\n%s", b.String())
	}
}
