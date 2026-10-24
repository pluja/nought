// SPDX-License-Identifier: AGPL-3.0-or-later

package imapsync

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/imaptest"
)

const raw = "From: a@example.org\r\nMessage-ID: <1@x>\r\nSubject: hi\r\n\r\nbody\r\n"

func dial(t *testing.T, s *imaptest.Server) *Conn {
	t.Helper()
	c, err := Dial(t.Context(), s.Config(), s.Client, imaptest.User, imaptest.Password, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestFetchFlow(t *testing.T) {
	s := imaptest.Start(t, imaptest.Options{})
	date := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	uid1 := s.Append(t, "INBOX", raw, date, imap.FlagSeen)
	uid2 := s.Append(t, "INBOX", strings.Replace(raw, "<1@x>", "<2@x>", 1), date, imap.FlagFlagged)
	s.CreateMailbox(t, "Archive/2024")

	c := dial(t, s)
	if !c.HasUIDPlus() || !slices.Contains(c.Capabilities(), "UIDPLUS") {
		t.Errorf("caps = %v", c.Capabilities())
	}
	folders, err := c.Folders()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range folders {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"Archive/2024", "INBOX", "Sent", "Trash"}) {
		t.Errorf("folders = %v", names)
	}

	if _, err := c.UIDsAfter(0); err == nil {
		t.Error("UIDsAfter without a selected folder succeeded")
	}
	mb, err := c.Select("INBOX", false)
	if err != nil || mb.Messages != 2 || mb.UIDValidity == 0 {
		t.Fatalf("select = %+v, %v", mb, err)
	}
	uids, err := c.UIDsAfter(0)
	if err != nil || !slices.Equal(uids, []uint32{uid1, uid2}) {
		t.Fatalf("uids = %v, %v", uids, err)
	}
	infos, err := c.Infos(uids)
	if err != nil || len(infos) != 2 {
		t.Fatalf("infos = %+v, %v", infos, err)
	}
	if infos[0].UID != uid1 || !infos[0].Seen || infos[1].UID != uid2 || !infos[1].Flagged ||
		!infos[0].InternalDate.Equal(date) || infos[0].Size != int64(len(raw)) {
		t.Errorf("infos = %+v", infos)
	}
	if later, err := c.UIDsAfter(uid2); err != nil || len(later) != 0 {
		t.Errorf("uids after last = %v, %v", later, err)
	}

	body, err := c.Fetch(uid2, 1<<20)
	if err != nil || !strings.Contains(string(body), "<2@x>") {
		t.Fatalf("fetch = %q, %v", body, err)
	}
	// Fetching with PEEK must not mark the message read.
	again, _ := c.Infos([]uint32{uid2})
	if again[0].Seen {
		t.Error("fetch set \\Seen")
	}
	if _, err := c.Fetch(9999, 1<<20); !errors.Is(err, ErrGone) {
		t.Errorf("fetch missing = %v", err)
	}
	found, err := c.Lookup([]uint32{uid1, 9999})
	if err != nil || len(found) != 1 || found[uid1].Size != int64(len(raw)) {
		t.Errorf("lookup = %+v, %v", found, err)
	}
	if empty, err := c.Lookup(nil); err != nil || len(empty) != 0 {
		t.Errorf("empty lookup = %+v, %v", empty, err)
	}
}

func TestExamine(t *testing.T) {
	// imapmemserver does not enforce EXAMINE's read-only mode, so this only
	// checks that a read-only select works; sync's dry run never expunges.
	s := imaptest.Start(t, imaptest.Options{})
	s.Append(t, "INBOX", raw, time.Now())
	c := dial(t, s)
	if mb, err := c.Select("INBOX", true); err != nil || mb.Messages != 1 {
		t.Fatalf("examine = %+v, %v", mb, err)
	}
	if uids, err := c.UIDsAfter(0); err != nil || len(uids) != 1 {
		t.Errorf("uids = %v, %v", uids, err)
	}
}

func TestExpungeOnlyGivenUIDs(t *testing.T) {
	s := imaptest.Start(t, imaptest.Options{})
	now := time.Now()
	keep := s.Append(t, "INBOX", raw, now)
	drop := s.Append(t, "INBOX", raw, now)
	other := s.Append(t, "INBOX", raw, now)
	// Another client marked a message for deletion; we must not expunge it.
	s.SetDeleted(t, "INBOX", other)

	c := dial(t, s)
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge(nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge([]uint32{drop}); err != nil {
		t.Fatal(err)
	}
	if got := s.UIDs(t, "INBOX"); !slices.Equal(got, []uint32{keep, other}) {
		t.Errorf("remaining = %v, want %v", got, []uint32{keep, other})
	}
}

func TestExpungeRequiresUIDPlus(t *testing.T) {
	s := imaptest.Start(t, imaptest.Options{NoUIDPlus: true})
	uid := s.Append(t, "INBOX", raw, time.Now())
	c := dial(t, s)
	if c.HasUIDPlus() {
		t.Fatal("server advertises UIDPLUS")
	}
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge([]uint32{uid}); !errors.Is(err, ErrNoUIDPlus) {
		t.Fatalf("err = %v", err)
	}
	if got := s.UIDs(t, "INBOX"); len(got) != 1 {
		t.Errorf("message deleted without UIDPLUS: %v", got)
	}
}

func TestDialErrors(t *testing.T) {
	s := imaptest.Start(t, imaptest.Options{})
	if _, err := Dial(t.Context(), s.Config(), s.Client, imaptest.User, "wrong", 0); err == nil {
		t.Error("bad password accepted")
	}
	// Without trusting the test certificate, verification must fail.
	if _, err := Dial(t.Context(), s.Config(), &tls.Config{}, imaptest.User, imaptest.Password, 0); err == nil {
		t.Error("untrusted certificate accepted")
	}
	bad := s.Config()
	bad.TLS = "none"
	if _, err := Dial(t.Context(), bad, s.Client, imaptest.User, imaptest.Password, 0); err == nil {
		t.Error("plaintext mode accepted")
	}
	// STARTTLS against an implicit-TLS port fails rather than downgrading.
	starttls := s.Config()
	starttls.TLS = config.TLSStartTLS
	if _, err := Dial(t.Context(), starttls, s.Client, imaptest.User, imaptest.Password, 0); err == nil {
		t.Error("starttls against a TLS port succeeded")
	}
}

func TestBrokenConnection(t *testing.T) {
	s := imaptest.Start(t, imaptest.Options{})
	uid := s.Append(t, "INBOX", raw, time.Now())
	c := dial(t, s)
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	s.BreakNextConnection(200)
	c2, err := Dial(t.Context(), s.Config(), s.Client, imaptest.User, imaptest.Password, 0)
	if err == nil {
		if _, err = c2.Select("INBOX", false); err == nil {
			_, err = c2.Fetch(uid, 1<<20)
		}
		_ = c2.Close()
	}
	if err == nil {
		t.Error("broken connection went unnoticed")
	}
}

func TestContextCancelClosesConnection(t *testing.T) {
	s := imaptest.Start(t, imaptest.Options{})
	ctx, cancel := context.WithCancel(t.Context())
	c, err := Dial(ctx, s.Config(), s.Client, imaptest.User, imaptest.Password, 0)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	// No command is sent after a cancel (Ctrl-C), not even one that would
	// win the race with the connection closing: an expunge least of all.
	if _, err := c.Folders(); !errors.Is(err, context.Canceled) {
		t.Errorf("Folders after cancel = %v", err)
	}
	if err := c.Expunge([]uint32{1}); !errors.Is(err, context.Canceled) {
		t.Errorf("Expunge after cancel = %v", err)
	}
	_ = c.Close()
}

// fakeServer runs a scripted IMAP server: it greets, accepts LOGIN and
// SELECT, and hands every other command line to respond.
func fakeServer(t *testing.T, respond func(w io.Writer, tag, line string)) (config.Server, *tls.Config) {
	t.Helper()
	cert, client := imaptest.Certificate(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, _ = io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1 UIDPLUS] fake\r\n")
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					tag, line, _ := strings.Cut(sc.Text(), " ")
					switch {
					case strings.HasPrefix(line, "LOGIN"):
						_, _ = io.WriteString(conn, tag+" OK [CAPABILITY IMAP4rev1 UIDPLUS] logged in\r\n")
					case strings.HasPrefix(line, "SELECT"), strings.HasPrefix(line, "EXAMINE"):
						_, _ = io.WriteString(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] x\r\n"+tag+" OK [READ-WRITE] done\r\n")
					case strings.HasPrefix(line, "LOGOUT"):
						_, _ = io.WriteString(conn, "* BYE\r\n"+tag+" OK bye\r\n")
						return
					default:
						respond(conn, tag, line)
					}
				}
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	return config.Server{Host: "127.0.0.1", Port: port, TLS: config.TLSImplicit}, client
}

func TestSilentServerTimesOut(t *testing.T) {
	srv, client := fakeServer(t, func(io.Writer, string, string) {}) // never answers
	c, err := Dial(t.Context(), srv, client, "u", "p", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.UIDsAfter(0)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("silent server: no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("hung on a silent server")
	}
	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung on a dead connection")
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	big := strings.Repeat("x", 5000)
	srv, client := fakeServer(t, func(w io.Writer, tag, line string) {
		if strings.Contains(line, "FETCH") {
			// Announces nothing about size, then sends 5000 bytes.
			_, _ = io.WriteString(w, fmt.Sprintf("* 1 FETCH (UID 1 BODY[] {%d}\r\n%s)\r\n%s OK done\r\n", len(big), big, tag))
		}
	})
	c, err := Dial(t.Context(), srv, client, "u", "p", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(1, 100); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	// The connection is still usable.
	body, err := c.Fetch(1, 10000)
	if err != nil || len(body) != len(big) {
		t.Errorf("within the limit: %d bytes, %v", len(body), err)
	}
}

func TestDialCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Dial(ctx, config.Server{Host: "127.0.0.1", Port: 1, TLS: config.TLSImplicit}, nil, "u", "p", 0); err == nil {
		t.Fatal("cancelled dial succeeded")
	}
}

func TestHasFlagIgnoresCase(t *testing.T) {
	if !hasFlag([]imap.Flag{"\\SEEN"}, imap.FlagSeen) || hasFlag(nil, imap.FlagSeen) {
		t.Error("hasFlag")
	}
}

func TestHeaders(t *testing.T) {
	s := imaptest.Start(t, imaptest.Options{})
	uid := s.Append(t, "INBOX", raw, time.Now())
	c := dial(t, s)
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	h, err := c.Headers([]uint32{uid, 9999})
	if err != nil || len(h) != 1 || !strings.HasPrefix(string(h[uid]), "From: a@example.org") || strings.Contains(string(h[uid]), "body") {
		t.Fatalf("headers = %v, %v", h, err)
	}
	infos, _ := c.Infos([]uint32{uid})
	if infos[0].Seen {
		t.Error("fetching headers set \\Seen")
	}
}

func TestAllUIDsAfterMovesAndExpunges(t *testing.T) {
	s := imaptest.Start(t, imaptest.Options{})
	s.CreateMailbox(t, "Archive")
	date := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	a := s.Append(t, "INBOX", raw, date)
	b := s.Append(t, "INBOX", strings.Replace(raw, "<1@x>", "<2@x>", 1), date)
	c := s.Append(t, "INBOX", strings.Replace(raw, "<1@x>", "<3@x>", 1), date)
	s.Move(t, "INBOX", a, "Archive")
	s.Copy(t, "INBOX", b, "Archive")
	s.Expunge(t, "INBOX", c)

	conn := dial(t, s)
	for mailbox, want := range map[string]int{"INBOX": 1, "Archive": 2} {
		if _, err := conn.Select(mailbox, true); err != nil {
			t.Fatal(err)
		}
		uids, err := conn.AllUIDs()
		if err != nil || len(uids) != want || !slices.IsSorted(uids) {
			t.Errorf("%s: uids %v, %v; want %d", mailbox, uids, err, want)
		}
	}
	if uids, _ := conn.AllUIDs(); slices.Contains(uids, 0) {
		t.Errorf("bad uid in %v", uids)
	}
	s.DeleteMailbox(t, "Archive")
	if folders, _ := conn.Folders(); slices.ContainsFunc(folders, func(f Folder) bool { return f.Name == "Archive" }) {
		t.Error("deleted mailbox still listed")
	}
}

// A UID list shorter than the folder's message count is never taken for
// the folder's contents.
func TestAllUIDsRejectsAnIncompleteList(t *testing.T) {
	srv, client := fakeServer(t, func(w io.Writer, tag, line string) {
		if strings.Contains(line, "SEARCH") {
			_, _ = io.WriteString(w, "* SEARCH\r\n"+tag+" OK done\r\n") // the folder has 1 message
		}
	})
	c, err := Dial(t.Context(), srv, client, "u", "p", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Select("INBOX", true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AllUIDs(); !errors.Is(err, ErrIncompleteList) {
		t.Errorf("AllUIDs = %v, want ErrIncompleteList", err)
	}
}

func TestExists(t *testing.T) {
	s := imaptest.Start(t, imaptest.Options{})
	c := dial(t, s)
	for name, want := range map[string]bool{"INBOX": true, "Sent": true, "Nope": false} {
		if got, err := c.Exists(name); err != nil || got != want {
			t.Errorf("Exists(%q) = %v, %v; want %v", name, got, err, want)
		}
	}
}

func TestAppend(t *testing.T) {
	s := imaptest.Start(t, imaptest.Options{})
	c := dial(t, s)
	date := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	if err := c.Append("Sent", []string{"Seen"}, date, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("Sent", true); err != nil {
		t.Fatal(err)
	}
	uids, err := c.AllUIDs()
	if err != nil {
		t.Fatal(err)
	}
	infos, err := c.Infos(uids)
	if err != nil || len(infos) != 1 || !infos[0].Seen || !infos[0].InternalDate.Equal(date) {
		t.Errorf("appended = %+v, %v", infos, err)
	}
}

// expungeServer scripts the commands Expunge sends, recording them: UID 1
// is ours to delete, UID 2 was already marked deleted by another client.
func expungeServer(t *testing.T, storeOK, expungeOK bool) (*Conn, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv, client := fakeServer(t, func(w io.Writer, tag, line string) {
		mu.Lock()
		seen = append(seen, line)
		mu.Unlock()
		reply := func(ok bool) string {
			if ok {
				return tag + " OK done\r\n"
			}
			return tag + " NO refused\r\n"
		}
		switch {
		case strings.Contains(line, "FETCH"):
			_, _ = io.WriteString(w, `* 1 FETCH (UID 1 FLAGS () RFC822.SIZE 10 INTERNALDATE "01-Jan-2026 00:00:00 +0000")`+"\r\n"+
				`* 2 FETCH (UID 2 FLAGS (\Deleted) RFC822.SIZE 10 INTERNALDATE "01-Jan-2026 00:00:00 +0000")`+"\r\n"+tag+" OK done\r\n")
		case strings.Contains(line, "STORE") && strings.Contains(line, "-FLAGS"):
			_, _ = io.WriteString(w, reply(true))
		case strings.Contains(line, "STORE"):
			_, _ = io.WriteString(w, reply(storeOK))
		case strings.Contains(line, "EXPUNGE"):
			_, _ = io.WriteString(w, reply(expungeOK))
		default:
			_, _ = io.WriteString(w, tag+" BAD unexpected\r\n")
		}
	})
	c, err := Dial(t.Context(), srv, client, "u", "p", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// Expunge's safety: only UID EXPUNGE of the given UIDs, never a plain
// EXPUNGE (which would delete every \Deleted message in the folder); no
// expunge if marking failed; and if the expunge fails, only the flags nought
// set are taken back, not those another client set.
func TestExpungeFailures(t *testing.T) {
	has := func(cmds []string, sub string) bool {
		return slices.ContainsFunc(cmds, func(c string) bool { return strings.Contains(c, sub) })
	}

	c, seen := expungeServer(t, true, true)
	if err := c.Expunge([]uint32{1, 2}); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range seen() {
		if cmd == "EXPUNGE" {
			t.Error("a plain EXPUNGE was sent")
		}
	}
	if !has(seen(), "UID EXPUNGE 1:2") {
		t.Errorf("commands = %q", seen())
	}

	c, seen = expungeServer(t, false, true)
	if err := c.Expunge([]uint32{1, 2}); err == nil || has(seen(), "EXPUNGE") {
		t.Errorf("after a failed STORE: err %v, commands %q", err, seen())
	}

	c, seen = expungeServer(t, true, false)
	if err := c.Expunge([]uint32{1, 2}); err == nil {
		t.Fatal("a failed UID EXPUNGE reported success")
	}
	if !has(seen(), "UID STORE 1 -FLAGS.SILENT") || has(seen(), "UID STORE 1:2 -FLAGS") {
		t.Errorf("flags taken back: %q", seen())
	}

	if err := c.Expunge(nil); err != nil {
		t.Errorf("empty: %v", err)
	}
	if err := c.Expunge(make([]uint32, MaxBatch+1)); err == nil {
		t.Error("an oversized batch was accepted")
	}
}

// Folders turns the server's list into names nought uses: "/" for any
// separator, \Noselect skipped, and the special-use attributes recorded
// whatever the folder is called.
func TestFoldersFromTheServer(t *testing.T) {
	srv, client := fakeServer(t, func(w io.Writer, tag, line string) {
		if strings.HasPrefix(line, "LIST") {
			_, _ = io.WriteString(w, `* LIST (\HasNoChildren) "." "INBOX"`+"\r\n"+
				`* LIST (\Noselect) "." "Archive"`+"\r\n"+
				`* LIST (\HasNoChildren) "." "Archive.2024"`+"\r\n"+
				`* LIST (\Trash) "." "Papelera"`+"\r\n"+
				`* LIST (\Drafts) "." "Borradores"`+"\r\n"+
				`* LIST (\All) "." "[Gmail].All Mail"`+"\r\n"+
				`* LIST (\Sent) "." "Enviados"`+"\r\n"+tag+" OK done\r\n")
		}
	})
	c, err := Dial(t.Context(), srv, client, "u", "p", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	folders, err := c.Folders()
	if err != nil {
		t.Fatal(err)
	}
	want := []Folder{
		{Name: "Archive/2024", Raw: "Archive.2024"},
		{Name: "Borradores", Raw: "Borradores", Special: true},
		{Name: "Enviados", Raw: "Enviados", Sent: true},
		{Name: "INBOX", Raw: "INBOX"},
		{Name: "Papelera", Raw: "Papelera", Trash: true},
		{Name: "[Gmail]/All Mail", Raw: "[Gmail].All Mail", Special: true},
	}
	if !slices.Equal(folders, want) {
		t.Errorf("folders:\n%+v\nwant:\n%+v", folders, want)
	}

	srv, client = fakeServer(t, func(w io.Writer, tag, line string) {
		_, _ = io.WriteString(w, tag+" NO not today\r\n")
	})
	c2, err := Dial(t.Context(), srv, client, "u", "p", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	if _, err := c2.Folders(); err == nil {
		t.Error("a refused LIST gave no error")
	}
}

// After a cancel, no command reaches the server; a server that refuses
// everything gives errors, never empty successes; commands that need a
// folder say so before one is selected.
func TestCommandsRefuseAndFail(t *testing.T) {
	calls := func(c *Conn) map[string]error {
		_, ex := c.Exists("x")
		_, sel := c.Select("INBOX", false)
		_, after := c.UIDsAfter(0)
		_, all := c.AllUIDs()
		_, look := c.Lookup([]uint32{1})
		_, infos := c.Infos([]uint32{1})
		_, fetch := c.Fetch(1, 100)
		_, heads := c.Headers([]uint32{1})
		return map[string]error{"Exists": ex, "Select": sel, "UIDsAfter": after, "AllUIDs": all, "Lookup": look,
			"Infos": infos, "Fetch": fetch, "Headers": heads, "Append": c.Append("INBOX", nil, time.Time{}, []byte("x"))}
	}
	refuse := func(w io.Writer, tag, line string) { _, _ = io.WriteString(w, tag+" NO refused\r\n") }

	srv, client := fakeServer(t, refuse)
	ctx, cancel := context.WithCancel(t.Context())
	c, err := Dial(ctx, srv, client, "u", "p", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for name, err := range calls(c) {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s after cancel: %v", name, err)
		}
	}

	for _, selectFirst := range []bool{false, true} {
		srv, client = fakeServer(t, refuse)
		c, err := Dial(t.Context(), srv, client, "u", "p", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if selectFirst {
			if _, err := c.Select("INBOX", false); err != nil {
				t.Fatal(err)
			}
		}
		for name, err := range calls(c) {
			// A plain NO to STATUS means no such folder: Exists is false.
			if err == nil && name != "Select" && name != "Exists" {
				t.Errorf("%s (folder selected: %v): no error from a refusing server", name, selectFirst)
			}
		}
		_ = c.Close()
	}
}

// A NO to STATUS means the folder is gone only when the server says so (or
// says nothing more): sync takes a vanished folder's mail as deleted on
// another device. A busy or broken server is an error, not a deletion.
func TestExistsTellsGoneFromUnavailable(t *testing.T) {
	for _, tt := range []struct {
		reply   string
		exists  bool
		wantErr bool
	}{
		{"OK [x] done", true, false},
		{"NO no such folder", false, false},
		{"NO [NONEXISTENT] no such folder", false, false},
		{"NO [UNAVAILABLE] try later", false, true},
		{"NO [INUSE] busy", false, true},
		{"NO [SERVERBUG] oops", false, true},
	} {
		srv, client := fakeServer(t, func(w io.Writer, tag, line string) {
			if strings.HasPrefix(line, "STATUS") {
				_, _ = io.WriteString(w, `* STATUS "x" (MESSAGES 1)`+"\r\n"+tag+" "+tt.reply+"\r\n")
			}
		})
		c, err := Dial(t.Context(), srv, client, "u", "p", 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.Exists("x")
		if got != tt.exists || (err != nil) != tt.wantErr {
			t.Errorf("%q: exists %v, err %v", tt.reply, got, err)
		}
		_ = c.Close()
	}
}

// Review: a server answering BODY[] NIL (legal IMAP, for a message it
// cannot read) crashed nou: the literal is nil. It is an empty body, and
// a header that is NIL is no header.
func TestNilBodyIsNotACrash(t *testing.T) {
	srv, client := fakeServer(t, func(w io.Writer, tag, line string) {
		switch {
		case strings.Contains(line, "BODY.PEEK[HEADER]"):
			_, _ = io.WriteString(w, "* 1 FETCH (UID 1 BODY[HEADER] NIL)\r\n"+tag+" OK done\r\n")
		case strings.Contains(line, "BODY.PEEK[]"):
			_, _ = io.WriteString(w, "* 1 FETCH (UID 1 BODY[] NIL)\r\n"+tag+" OK done\r\n")
		}
	})
	c, err := Dial(t.Context(), srv, client, "u", "p", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	if body, err := c.Fetch(1, 1000); err != nil || len(body) != 0 {
		t.Errorf("Fetch of a NIL body = %q, %v; want empty", body, err)
	}
	if headers, err := c.Headers([]uint32{1}); err != nil || len(headers) != 0 {
		t.Errorf("Headers with a NIL header = %v, %v; want none", headers, err)
	}
}

// A server that answers NO to fetching one message (it cannot read it)
// says the message is unavailable, so sync can carry on with the others;
// a broken connection is not.
func TestServerNoIsUnavailable(t *testing.T) {
	srv, client := fakeServer(t, func(w io.Writer, tag, line string) {
		if strings.Contains(line, "FETCH") {
			_, _ = io.WriteString(w, tag+" NO [SERVERBUG] cannot read that message\r\n")
		}
	})
	c, err := Dial(t.Context(), srv, client, "u", "p", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(1, 1000); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Fetch answered NO = %v, want ErrUnavailable", err)
	}
}
