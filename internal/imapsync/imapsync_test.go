// SPDX-License-Identifier: AGPL-3.0-or-later

package imapsync

import (
	"context"
	"crypto/tls"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"nought/internal/config"
	"nought/internal/imaptest"
)

const raw = "From: a@example.org\r\nMessage-ID: <1@x>\r\nSubject: hi\r\n\r\nbody\r\n"

func dial(t *testing.T, s *imaptest.Server) *Conn {
	t.Helper()
	c, err := Dial(t.Context(), s.Config(), s.Client, imaptest.User, imaptest.Password)
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

	if _, err := c.List(0); err == nil {
		t.Error("List without a selected folder succeeded")
	}
	mb, err := c.Select("INBOX")
	if err != nil || mb.Messages != 2 || mb.UIDValidity == 0 {
		t.Fatalf("select = %+v, %v", mb, err)
	}
	infos, err := c.List(0)
	if err != nil || len(infos) != 2 {
		t.Fatalf("list = %+v, %v", infos, err)
	}
	if infos[0].UID != uid1 || !infos[0].Seen || infos[1].UID != uid2 || !infos[1].Flagged ||
		!infos[0].InternalDate.Equal(date) || infos[0].Size != int64(len(raw)) {
		t.Errorf("infos = %+v", infos)
	}
	if later, err := c.List(uid2); err != nil || len(later) != 0 {
		t.Errorf("list after last uid = %+v, %v", later, err)
	}

	body, err := c.Fetch(uid1)
	if err != nil || string(body) != raw {
		t.Fatalf("fetch = %q, %v", body, err)
	}
	// Fetching with PEEK must not mark the message read.
	again, _ := c.List(0)
	if again[1].Seen {
		t.Error("fetch set \\Seen")
	}
	if _, err := c.Fetch(9999); !errors.Is(err, ErrGone) {
		t.Errorf("fetch missing = %v", err)
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
	if _, err := c.Select("INBOX"); err != nil {
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
	if _, err := c.Select("INBOX"); err != nil {
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
	if _, err := Dial(t.Context(), s.Config(), s.Client, imaptest.User, "wrong"); err == nil {
		t.Error("bad password accepted")
	}
	// Without trusting the test certificate, verification must fail.
	if _, err := Dial(t.Context(), s.Config(), &tls.Config{}, imaptest.User, imaptest.Password); err == nil {
		t.Error("untrusted certificate accepted")
	}
	bad := s.Config()
	bad.TLS = "none"
	if _, err := Dial(t.Context(), bad, s.Client, imaptest.User, imaptest.Password); err == nil {
		t.Error("plaintext mode accepted")
	}
	// STARTTLS against an implicit-TLS port fails rather than downgrading.
	starttls := s.Config()
	starttls.TLS = config.TLSStartTLS
	if _, err := Dial(t.Context(), starttls, s.Client, imaptest.User, imaptest.Password); err == nil {
		t.Error("starttls against a TLS port succeeded")
	}
}

func TestBrokenConnection(t *testing.T) {
	s := imaptest.Start(t, imaptest.Options{})
	uid := s.Append(t, "INBOX", raw, time.Now())
	c := dial(t, s)
	if _, err := c.Select("INBOX"); err != nil {
		t.Fatal(err)
	}
	s.BreakNextConnection(200)
	c2, err := Dial(t.Context(), s.Config(), s.Client, imaptest.User, imaptest.Password)
	if err == nil {
		if _, err = c2.Select("INBOX"); err == nil {
			_, err = c2.Fetch(uid)
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
	c, err := Dial(ctx, s.Config(), s.Client, imaptest.User, imaptest.Password)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := c.Folders(); err == nil {
		t.Error("connection still usable after cancel")
	}
	_ = c.Close()
}
