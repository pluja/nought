// SPDX-License-Identifier: AGPL-3.0-or-later

// Command seed fills the local test IMAP server (see testenv/) with a
// varied mailbox for alice and bob: nested and non-ASCII folders, mail
// spread over time, HTML, attachments, legacy charsets, broken MIME, hostile
// headers, duplicates and a large message. Development only; it refuses to
// run outside NOU_DEV=1 and only talks to 127.0.0.1.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/pluja/nought/internal/demo"
)

const (
	addr     = "127.0.0.1:10993"
	password = "nought-test-password"
)

func main() {
	force := flag.Bool("force", false, "seed even if INBOX already has mail")
	demoUser := flag.String("demo", "", "fill this (fresh) user with the README demo mailbox instead")
	flag.Parse()
	if os.Getenv("NOU_DEV") != "1" {
		log.Fatal("seed: run via 'make testenv-seed' (NOU_DEV=1)")
	}
	tlsConfig, err := testTLS(os.Getenv("NOU_DEV_CA"))
	if err != nil {
		log.Fatal(err)
	}
	if *demoUser != "" {
		if err := seedDemo(*demoUser, tlsConfig); err != nil {
			log.Fatalf("%s: %v", *demoUser, err)
		}
		return
	}
	for _, user := range []string{"alice@nought.test", "bob@nought.test"} {
		n, err := seedUser(user, tlsConfig, *force)
		if err != nil {
			log.Fatalf("%s: %v", user, err)
		}
		fmt.Printf("%s: %d messages added\n", user, n)
	}
}

func testTLS(caFile string) (*tls.Config, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("NOU_DEV_CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("NOU_DEV_CA: no certificate")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

type msg struct {
	folder string
	age    time.Duration
	flags  []imap.Flag
	raw    string
}

func seedUser(user string, tlsConfig *tls.Config, force bool) (int, error) {
	c, err := imapclient.DialTLS(addr, &imapclient.Options{TLSConfig: tlsConfig})
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close() }()
	if err := c.Login(user, password).Wait(); err != nil {
		return 0, err
	}
	box, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return 0, err
	}
	if box.NumMessages > 0 && !force {
		return 0, nil
	}
	for _, folder := range []string{"Archive", "Archive.2024", "Projects", "Projects.Café ☕", "Newsletters"} {
		if err := c.Create(folder, nil).Wait(); err != nil && !strings.Contains(err.Error(), "exists") {
			return 0, fmt.Errorf("create %s: %w", folder, err)
		}
	}
	msgs := messages(user)
	for _, m := range msgs {
		raw := strings.ReplaceAll(m.raw, "\n", "\r\n")
		cmd := c.Append(m.folder, int64(len(raw)), &imap.AppendOptions{Flags: m.flags, Time: time.Now().Add(-m.age)})
		if _, err := cmd.Write([]byte(raw)); err != nil {
			return 0, err
		}
		if err := cmd.Close(); err != nil {
			return 0, err
		}
		if _, err := cmd.Wait(); err != nil {
			return 0, fmt.Errorf("append to %s: %w", m.folder, err)
		}
	}
	return len(msgs), c.Logout().Wait()
}

// seedDemo fills a user with the made-up demo mailbox (internal/demo).
func seedDemo(user string, tlsConfig *tls.Config) error {
	c, err := imapclient.DialTLS(addr, &imapclient.Options{TLSConfig: tlsConfig})
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if err := c.Login(user, password).Wait(); err != nil {
		return err
	}
	if err := c.Create("Archive", nil).Wait(); err != nil && !strings.Contains(err.Error(), "exists") {
		return fmt.Errorf("create Archive: %w", err)
	}
	for _, m := range demo.Mailbox(time.Now()) {
		var flags []imap.Flag
		if m.Read {
			flags = append(flags, imap.FlagSeen)
		}
		cmd := c.Append(m.Folder, int64(len(m.Raw)), &imap.AppendOptions{Flags: flags, Time: m.Date})
		if _, err := cmd.Write([]byte(m.Raw)); err != nil {
			return err
		}
		if err := cmd.Close(); err != nil {
			return err
		}
		if _, err := cmd.Wait(); err != nil {
			return fmt.Errorf("append to %s: %w", m.Folder, err)
		}
	}
	return c.Logout().Wait()
}

const day = 24 * time.Hour

func messages(user string) []msg {
	date := func(age time.Duration) string { return time.Now().Add(-age).Format(time.RFC1123Z) }
	plain := func(id, from, subject string, age time.Duration, body string) string {
		return fmt.Sprintf("From: %s\nTo: %s\nDate: %s\nMessage-ID: <%s@nought.test>\nSubject: %s\n\n%s\n",
			from, user, date(age), id, subject, body)
	}
	seen := []imap.Flag{imap.FlagSeen}
	big := strings.Repeat("All work and no play makes Jack a dull boy.\n", 120000) // ~5 MB

	return []msg{
		{"INBOX", 2 * time.Hour, nil, plain("fresh-1", "Carol <carol@example.org>", "Lunch on Friday?", 2*time.Hour, "Pizza at noon?")},
		{"INBOX", 3 * day, nil, plain("fresh-2", "Dave <dave@example.org>", "Invoice 2026-114", 3*day, "Please find the invoice details below.\nTotal: 120 EUR")},
		{"INBOX", 20 * day, seen, plain("old-1", "Erin <erin@example.org>", "Re: quarterly report", 20*day, "> numbers?\nThey are up 12%.")},
		{"INBOX", 45 * day, seen, plain("old-2", "Frank <frank@example.org>", "Old thread", 45*day, "This one should leave the server once deletion is on.")},
		{"INBOX", 5 * day, []imap.Flag{imap.FlagSeen, imap.FlagFlagged}, plain("flagged", "Grace <grace@example.org>", "Important: contract", 5*day, "Flagged on the server.")},
		{"INBOX", 1 * day, nil, fmt.Sprintf(`From: Mallory <mallory@example.org>
To: %s
Date: %s
Message-ID: <hostile@nought.test>
Subject: =?UTF-8?Q?Urgent_=1B[31mACTION=1B[0m_required_=E2=80=AE?=

Terminal escapes in the subject must be neutralized.
`, user, date(day))},
		{"INBOX", 4 * day, nil, fmt.Sprintf(`From: =?ISO-8859-1?Q?J=F6rg_M=FCller?= <joerg@example.de>
To: %s
Date: %s
Message-ID: <latin1@nought.test>
Subject: =?ISO-8859-1?Q?Gr=FC=DFe_aus_K=F6ln?=
Content-Type: text/plain; charset=iso-8859-1
Content-Transfer-Encoding: quoted-printable

Sch=F6ne Gr=FC=DFe!
`, user, date(4*day))},
		{"INBOX", 6 * day, nil, fmt.Sprintf(`From: Broken <broken@example.org>
To: %s
Date: not a date
Subject: broken MIME
Content-Type: multipart/mixed; boundary="never-closed"

--never-closed
Content-Type: text/plain

the boundary never closes
`, user)},
		{"Newsletters", 10 * day, nil, fmt.Sprintf(`From: Weekly <news@example.com>
To: %s
Date: %s
Message-ID: <newsletter-1@nought.test>
Subject: This week in privacy
List-Unsubscribe: <https://example.com/unsub>
Content-Type: text/html; charset=utf-8

<html><body><h1>This week</h1><p>Read <a href="https://example.com/post">the post</a>.</p>
<img src="https://tracker.example.com/open.gif?u=123" width="1" height="1"><script>track()</script></body></html>
`, user, date(10*day))},
		{"Projects.Café ☕", 15 * day, seen, fmt.Sprintf(`From: Heidi <heidi@example.org>
To: %s
Date: %s
Message-ID: <attachment@nought.test>
Subject: Plans attached
Content-Type: multipart/mixed; boundary=b1

--b1
Content-Type: text/plain

See the attached plan.
--b1
Content-Type: application/pdf; name="plan.pdf"
Content-Disposition: attachment; filename="plan.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQKJSBmYWtlIHBkZgo=
--b1--
`, user, date(15*day))},
		{"Archive.2024", 400 * day, seen, plain("archived-2024", "Ivan <ivan@example.org>", "From last year", 400*day, "Old archived mail.")},
		{"Archive", 30 * day, seen, plain("fresh-2", "Dave <dave@example.org>", "Invoice 2026-114", 3*day, "Please find the invoice details below.\nTotal: 120 EUR")}, // duplicate of INBOX's
		{"Sent", 2 * day, seen, plain("sent-1", user, "Re: Lunch on Friday?", 2*day, "Sounds good!")},
		{"Drafts", 1 * day, []imap.Flag{imap.FlagDraft}, plain("draft-1", user, "unfinished draft", day, "Never archived or deleted.")},
		{"Junk", 1 * day, nil, plain("spam-1", "Prize <win@spam.example>", "You won!!!", day, "Never archived.")},
		{"Trash", 1 * day, seen, plain("trashed-1", "Old <old@example.org>", "deleted on the phone", day, "Never archived.")},
		{"INBOX", 8 * day, seen, plain("big", "Backup <backup@example.org>", "Large message (5 MB)", 8*day, big)},
	}
}
