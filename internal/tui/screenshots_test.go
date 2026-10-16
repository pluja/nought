// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pluja/nought/internal/config"
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
		"inbox": func(t *testing.T, m Model) Model { return m },
		"newsletter": func(t *testing.T, m Model) Model {
			return drive(t, m, key("j"), key("j"))
		},
		"review": func(t *testing.T, m Model) Model {
			m.editFile = fakeEditor(func(s string) string {
				return strings.Replace(s, "\n\n", "\n\nWonderful! I attached the tram tickets.\n\nSam", 1)
			})
			return drive(t, m, key("r"))
		},
		"sending": func(t *testing.T, m Model) Model {
			m.editFile = fakeEditor(func(s string) string {
				return strings.Replace(s, "\n\n", "\n\nWonderful, see you on Friday!\n\nSam", 1)
			})
			m = drive(t, m, key("r"))
			m = drive(t, m, key("y"))
			m.now = func() time.Time { return testNow.Add(3 * time.Second) }
			return m
		},
		"help": func(t *testing.T, m Model) Model { return drive(t, m, key("?")) },
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
	env := ops.NewEnv(paths, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return testNow },
		ops.BuildInfo{}, secret.Memory{})
	t.Cleanup(func() { _ = env.Close() })
	imapSrv, smtpSrv := imaptest.Start(t, imaptest.Options{}), smtptest.Start(t, smtptest.Options{})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(imapSrv.CAPEM)
	pool.AppendCertsFromPEM(smtpSrv.CAPEM)
	env.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	imapSrv.CreateMailbox(t, "Archive") // INBOX, Sent and Trash exist
	for _, d := range demoMail {
		imapSrv.Append(t, d.folder, demoMessage(d), testNow.Add(-d.age))
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
	for _, d := range demoMail {
		if d.read {
			_, err = ops.Call(t.Context(), env, ops.Delete, ops.MsgTag, ops.TagIn{Query: `subject:"` + d.subject + `"`, Remove: []string{ops.TagUnread}})
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

type demo struct {
	folder, from, subject, body string
	age                         time.Duration
	read                        bool
	html                        bool
	attach                      string
}

func demoMessage(d demo) string {
	id := strings.ToLower(regexp.MustCompile(`\W+`).ReplaceAllString(d.subject, "")) + "@demo.example"
	head := "From: " + d.from + "\r\nTo: Sam Rivera <sam@example.org>\r\nMessage-ID: <" + id + ">\r\nDate: " +
		testNow.Add(-d.age).Format(time.RFC1123Z) + "\r\nSubject: " + d.subject + "\r\nMIME-Version: 1.0\r\n"
	body := strings.ReplaceAll(d.body, "\n", "\r\n")
	switch {
	case d.html:
		return head + "Content-Type: text/html; charset=utf-8\r\n\r\n" + body
	case d.attach != "":
		return head + "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
			body + "\r\n--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=" + d.attach +
			"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n--b--\r\n"
	}
	return head + "Content-Type: text/plain; charset=utf-8\r\n\r\n" + body
}

const hour = time.Hour

var demoMail = []demo{
	{folder: "INBOX", from: "Alice Martin <alice@example.org>", subject: "Re: Weekend in Lisbon", age: 2 * hour, body: `That sounds perfect. I'll book the tram tour for Saturday morning,
and dinner at the place with the view on Friday.

Alice

On Tue, 13 Oct 2026 18:40, Sam Rivera wrote:
> Great! The hotel is in Alfama, five minutes from the viewpoint.
> Shall we do the tram tour?
>
> On Tue, 13 Oct 2026 09:15, Alice Martin wrote:
> > Flights are booked: we land on Friday at 10:05.
> > Can you find a place to stay?
`},
	{folder: "INBOX", from: "Bob Chen <bob@example.net>", subject: "Photos from Saturday", age: 5 * hour,
		body: "Here they are! The one at the lighthouse is my favourite.\n\nBob\n"},
	{folder: "INBOX", from: "The Weekly Byte <news@weeklybyte.example>", subject: "Issue #42: the return of the terminal", age: 20 * hour, html: true, body: `<html><body>
<h1>The return of the terminal</h1>
<p>Hello reader,</p>
<p>This week: why <b>keyboard-first</b> tools are everywhere again, and three you should try.</p>
<ul><li><a href="https://weeklybyte.example/a/1?utm_source=newsletter&amp;utm_medium=email">Plain text is a feature</a></li>
<li><a href="https://weeklybyte.example/a/2?utm_source=newsletter">Inbox Zero, revisited</a></li>
<li><a href="https://weeklybyte.example/a/3?utm_source=newsletter">Your files, your formats</a></li></ul>
<img src="https://tracker.weeklybyte.example/open.gif" width="1" height="1">
<p>Read the full issue on <a href="https://weeklybyte.example/42">our website</a>.</p>
<p style="font-size:11px;color:#999">You get this because you subscribed. <a href="https://weeklybyte.example/unsubscribe">Unsubscribe</a>.</p>
</body></html>`},
	{folder: "INBOX", from: "Hosting Co <billing@hostingco.example>", subject: "Invoice 2026-1015 (paid)", age: 26 * hour, read: true,
		attach: "invoice-2026-1015.pdf", body: "Thank you for your payment of 12.00 EUR.\n\nYour invoice is attached.\n"},
	{folder: "INBOX", from: "City Library <noreply@library.example>", subject: "Your books are due on Friday", age: 2 * 24 * hour, read: true,
		body: "Two books are due on Friday, 16 October. Renew them online.\n"},
	{folder: "INBOX", from: "Mia Okafor <mia@example.com>", subject: "Lunch next week?", age: 3 * 24 * hour, read: true,
		body: "Tuesday or Thursday work for me. The new ramen place?\n\nMia\n"},
	{folder: "INBOX", from: "Acme Accounts <no-reply@accounts.example>", subject: "Your sign-in code", age: 30 * time.Minute, read: true,
		body: "Your code is 481 516. It expires in 10 minutes.\n"},
	{folder: "Sent", from: "Sam Rivera <sam@example.org>", subject: "Re: Weekend in Lisbon (hotel)", age: 42 * hour, read: true,
		body: "Great! The hotel is in Alfama, five minutes from the viewpoint.\n"},
	{folder: "Archive", from: "Leo Park <leo@example.net>", subject: "Board game night", age: 9 * 24 * hour, read: true,
		body: "Bring snacks!\n"},
}
