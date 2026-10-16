// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/demo"
	"github.com/pluja/nought/internal/imaptest"
	"github.com/pluja/nought/internal/ops"
	"github.com/pluja/nought/internal/secret"
	"github.com/pluja/nought/internal/smtptest"
)

var screenshots = flag.String("screenshots", "", "write the README screens (ANSI) to this directory")

// TestScreenshots writes the README's screenshots from a made-up mailbox
// (make screenshots). No real mail is involved.
func TestScreenshots(t *testing.T) {
	if *screenshots == "" {
		t.Skip("run with -screenshots DIR (make screenshots)")
	}
	shots := map[string]func(t *testing.T, m Model) Model{
		"inbox": func(t *testing.T, m Model) Model { t.Helper(); return m },
		"newsletter": func(t *testing.T, m Model) Model {
			t.Helper()
			return drive(t, m, key("j"), key("j"))
		},
		"review": func(t *testing.T, m Model) Model {
			t.Helper()
			m.editFile = fakeEditor(func(s string) string {
				return strings.Replace(s, "\n\n", "\n\nWonderful! I attached the tram tickets.\n\nSam", 1)
			})
			return drive(t, m, key("r"))
		},
		"sending": func(t *testing.T, m Model) Model {
			t.Helper()
			m.editFile = fakeEditor(func(s string) string {
				return strings.Replace(s, "\n\n", "\n\nWonderful, see you on Friday!\n\nSam", 1)
			})
			m = drive(t, m, key("r"))
			m = drive(t, m, key("y"))
			m.now = func() time.Time { return testNow.Add(3 * time.Second) }
			return m
		},
		"help": func(t *testing.T, m Model) Model { t.Helper(); return drive(t, m, key("?")) },
	}
	for name, shoot := range shots {
		t.Run(name, func(t *testing.T) {
			m := shoot(t, demoModel(t, 124, 32))
			if err := os.WriteFile(filepath.Join(*screenshots, name+".ansi"), []byte(m.render()), 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// demoModel is the TUI over a made-up mailbox: Sam Rivera's, synced from a
// test IMAP server, with a test SMTP server for sending.
func demoModel(t *testing.T, w, h int) Model {
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
	env := ops.NewEnv(paths, slog.New(slog.DiscardHandler), func() time.Time { return testNow },
		ops.BuildInfo{}, secret.Memory{})
	t.Cleanup(func() { _ = env.Close() })
	imapSrv, smtpSrv := imaptest.Start(t, imaptest.Options{}), smtptest.Start(t, smtptest.Options{})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(imapSrv.CAPEM)
	pool.AppendCertsFromPEM(smtpSrv.CAPEM)
	env.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	imapSrv.CreateMailbox(t, "Archive") // INBOX, Sent and Trash exist
	mailbox := demo.Mailbox(testNow)
	for _, d := range mailbox {
		imapSrv.Append(t, d.Folder, d.Raw, d.Date)
	}
	call := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	imapCfg := imapSrv.Config()
	_, err = ops.Call(t.Context(), env, ops.Delete, ops.AccountAdd, ops.AccountIn{Name: "personal", Email: "sam@example.org",
		Username: imaptest.User, Password: imaptest.Password, IMAPHost: imapCfg.Host, IMAPPort: imapCfg.Port, IMAPTLS: imapCfg.TLS,
		SMTPHost: smtpSrv.Host, SMTPPort: smtpSrv.Port})
	call(err)
	_, err = ops.Call(t.Context(), env, ops.Delete, ops.AccountSet, ops.AccountSetIn{Name: "personal", Deletion: config.DeletionEnabled})
	call(err)
	_, err = ops.Call(t.Context(), env, ops.Delete, ops.SyncRun, ops.SyncIn{})
	call(err)
	for _, d := range mailbox {
		if d.Read {
			_, err = ops.Call(t.Context(), env, ops.Delete, ops.MsgTag, ops.TagIn{Query: `subject:"` + d.Subject + `"`, Remove: []string{ops.TagUnread}})
			call(err)
		}
	}
	for _, tag := range []ops.TagIn{
		{Query: `subject:lisbon`, Add: []string{"travel"}},
		{Query: `subject:invoice`, Add: []string{"bills", ops.TagFlagged}},
		{Query: `subject:"sign-in code"`, Add: []string{ops.TagDeleted}, Remove: []string{ops.TagInbox}},
	} {
		_, err = ops.Call(t.Context(), env, ops.Delete, ops.MsgTag, tag)
		call(err)
	}

	m := newModel(t.Context(), env)
	m.previewWait, m.syncEvery = 0, 0 // synced above
	m.after = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
	styles := m.input.Styles()
	styles.Cursor.Blink = false
	m.input.SetStyles(styles)
	return drive(t, m, tea.WindowSizeMsg{Width: w, Height: h}, initMsg{})
}
