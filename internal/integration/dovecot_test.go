// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

// Package integration runs nought against the real Dovecot server of the
// local test environment (make testenv-up; make integration). Each test
// uses a fresh, randomly named mailbox user, so runs never interfere.
package integration

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"nought/internal/config"
	"nought/internal/ops"
	"nought/internal/query"
	"nought/internal/secret"
)

const password = "nought-test-password"

func devTLS(t *testing.T) *tls.Config {
	t.Helper()
	if os.Getenv("NOU_DEV") != "1" {
		t.Skip("run via 'make integration' with the test environment up")
	}
	pem, err := os.ReadFile(os.Getenv("NOU_DEV_CA"))
	if err != nil {
		t.Fatalf("NOU_DEV_CA: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

type fixture struct {
	t    *testing.T
	env  *ops.Env
	user string
	tls  *tls.Config
}

func setup(t *testing.T, tlsMode string, port int) *fixture {
	t.Helper()
	tlsConfig := devTLS(t)
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
	env := ops.NewEnv(paths, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now,
		ops.BuildInfo{Version: "integration"}, secret.Memory{})
	env.TLS = tlsConfig
	t.Cleanup(func() { _ = env.Close() })
	f := &fixture{t: t, env: env, user: "it-" + strings.ToLower(rand.Text()[:10]) + "@nought.test", tls: tlsConfig}
	if _, err := ops.Call(t.Context(), env, ops.Delete, ops.AccountAdd, ops.AccountIn{
		Name: "it", Email: f.user, IMAPHost: "127.0.0.1", IMAPPort: port, IMAPTLS: tlsMode, Password: password,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) client() *imapclient.Client {
	f.t.Helper()
	c, err := imapclient.DialTLS("127.0.0.1:10993", &imapclient.Options{TLSConfig: f.tls})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := c.Login(f.user, password).Wait(); err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) append(mailbox, id, subject string, age time.Duration, flags ...imap.Flag) {
	f.t.Helper()
	c := f.client()
	defer func() { _ = c.Close() }()
	if mailbox != "INBOX" {
		_ = c.Create(mailbox, nil).Wait() // may exist
	}
	raw := "From: a@example.org\r\nTo: " + f.user + "\r\nMessage-ID: <" + id + "@it>\r\nSubject: " + subject +
		"\r\nDate: " + time.Now().Add(-age).Format(time.RFC1123Z) + "\r\n\r\nbody of " + subject + "\r\n"
	cmd := c.Append(mailbox, int64(len(raw)), &imap.AppendOptions{Flags: flags, Time: time.Now().Add(-age)})
	if _, err := cmd.Write([]byte(raw)); err != nil {
		f.t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		f.t.Fatal(err)
	}
	if _, err := cmd.Wait(); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(mailbox string) uint32 {
	f.t.Helper()
	c := f.client()
	defer func() { _ = c.Close() }()
	data, err := c.Status(mailbox, &imap.StatusOptions{NumMessages: true}).Wait()
	if err != nil {
		f.t.Fatal(err)
	}
	return *data.NumMessages
}

func (f *fixture) sync(dryRun bool) ops.SyncOut {
	f.t.Helper()
	out, err := ops.Call(f.t.Context(), f.env, ops.Delete, ops.SyncRun, ops.SyncIn{DryRun: dryRun})
	if err != nil {
		f.t.Fatalf("sync: %v\n%+v", err, out)
	}
	return out
}

func (f *fixture) subjects(q string) []string {
	f.t.Helper()
	ix, err := f.env.Index(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	parsed, err := query.Parse(q)
	if err != nil {
		f.t.Fatal(err)
	}
	msgs, err := ix.Search(f.t.Context(), parsed, 0, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, m := range msgs {
		out = append(out, m.Subject)
	}
	slices.Sort(out)
	return out
}

const day = 24 * time.Hour

func TestArchiveThenDeleteOnDovecot(t *testing.T) {
	for _, mode := range []struct {
		tls  string
		port int
	}{{config.TLSImplicit, 10993}, {config.TLSStartTLS, 10143}} {
		t.Run(mode.tls, func(t *testing.T) {
			f := setup(t, mode.tls, mode.port)
			f.append("INBOX", "old", "old mail", 30*day, imap.FlagSeen)
			f.append("INBOX", "new", "new mail", time.Hour)
			f.append("Archive.2024", "nested", "nested folder", 400*day)
			f.append("Drafts", "draft", "a draft", 30*day)

			dry := f.sync(true)
			if len(f.subjects("")) != 0 || f.count("INBOX") != 2 {
				t.Fatalf("dry run changed something: %+v", dry)
			}

			f.sync(false)
			if got := f.subjects(""); !slices.Equal(got, []string{"nested folder", "new mail", "old mail"}) {
				t.Errorf("archived = %v", got)
			}
			if got := f.subjects("tag:folder:archive/2024"); len(got) != 1 {
				t.Errorf("nested folder tag: %v", got)
			}
			if f.count("INBOX") != 2 {
				t.Fatal("deleted with deletion disabled")
			}

			if _, err := ops.Call(t.Context(), f.env, ops.Delete, ops.AccountSet,
				ops.AccountSetIn{Name: "it", Deletion: "enabled"}); err != nil {
				t.Fatal(err)
			}
			f.sync(false)
			if got := []uint32{f.count("INBOX"), f.count("Archive.2024"), f.count("Drafts")}; !slices.Equal(got, []uint32{1, 0, 1}) {
				t.Errorf("server counts INBOX/Archive.2024/Drafts = %v, want [1 0 1]", got)
			}
			if got := f.subjects(""); len(got) != 3 {
				t.Errorf("archive lost mail: %v", got)
			}
		})
	}
}

func TestDamagedArchiveKeepsServerCopy(t *testing.T) {
	f := setup(t, config.TLSImplicit, 10993)
	f.append("INBOX", "old", "precious", 30*day)
	f.sync(false)
	entries, err := os.ReadDir(f.env.Paths.AccountArchive("it") + "/cur")
	if err != nil || len(entries) != 1 {
		t.Fatalf("archive: %v %v", entries, err)
	}
	if err := os.WriteFile(f.env.Paths.AccountArchive("it")+"/cur/"+entries[0].Name(), []byte("rot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Call(t.Context(), f.env, ops.Delete, ops.AccountSet,
		ops.AccountSetIn{Name: "it", Deletion: "enabled"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Call(t.Context(), f.env, ops.Delete, ops.SyncRun, ops.SyncIn{}); err == nil {
		t.Error("sync with a damaged copy reported success")
	}
	if f.count("INBOX") != 1 {
		t.Fatal("deleted a message whose local copy was damaged")
	}
}
