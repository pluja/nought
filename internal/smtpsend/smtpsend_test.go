// SPDX-License-Identifier: AGPL-3.0-or-later

package smtpsend

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/smtptest"
)

func send(t *testing.T, srv *smtptest.Server, to []string, raw []byte) error {
	t.Helper()
	return Send(t.Context(), srv.Config(), srv.Client, smtptest.User, smtptest.Password,
		Envelope{From: "me@example.org", To: to}, raw, 5*time.Second)
}

func TestSendDeliversAllOrNothing(t *testing.T) {
	raw := []byte("From: me@example.org\r\nTo: a@example.org\r\nSubject: s\r\n\r\nhi\r\n")
	for _, starttls := range []bool{false, true} {
		srv := smtptest.Start(t, smtptest.Options{StartTLS: starttls})
		if err := send(t, srv, []string{"a@example.org", "b@example.org"}, raw); err != nil {
			t.Fatalf("starttls=%v: %v", starttls, err)
		}
		got := srv.Messages()
		if len(got) != 1 || len(got[0].To) != 2 || !bytes.Equal(got[0].Data, raw) || got[0].Auth != smtptest.User {
			t.Errorf("starttls=%v: received %+v", starttls, got)
		}
	}
	srv := smtptest.Start(t, smtptest.Options{RejectRcpt: "b@example.org"})
	err := send(t, srv, []string{"a@example.org", "b@example.org"}, []byte("x\r\n"))
	if !errors.Is(err, ErrPermanent) || len(srv.Messages()) != 0 {
		t.Errorf("refused recipient: err %v, delivered %d (want none)", err, len(srv.Messages()))
	}
}

// A connection lost while waiting for the reply to the message leaves its
// fate unknown: it may have been delivered, so it must not be retried.
func TestSendLostReplyIsUnknown(t *testing.T) {
	srv := smtptest.Start(t, smtptest.Options{DropData: true})
	err := send(t, srv, []string{"a@example.org"}, []byte("x\r\n"))
	if !errors.Is(err, ErrUnknown) || errors.Is(err, ErrTemporary) || len(srv.Messages()) != 1 {
		t.Errorf("lost reply: %v (%d received)", err, len(srv.Messages()))
	}
}

func TestSendErrorKinds(t *testing.T) {
	for code, want := range map[int]error{451: ErrTemporary, 554: ErrPermanent} {
		srv := smtptest.Start(t, smtptest.Options{FailData: code})
		if err := send(t, srv, []string{"a@example.org"}, []byte("x\r\n")); !errors.Is(err, want) {
			t.Errorf("%d: %v, want %v", code, err, want)
		}
	}
	srv := smtptest.Start(t, smtptest.Options{MaxSize: 10})
	if err := send(t, srv, []string{"a@example.org"}, bytes.Repeat([]byte("x"), 100)); !errors.Is(err, ErrPermanent) ||
		!strings.Contains(err.Error(), "too large") {
		t.Errorf("too large: %v", err)
	}
	bad := config.Server{Host: "127.0.0.1", Port: 1, TLS: config.TLSImplicit}
	if err := Send(t.Context(), bad, nil, "u", "p", Envelope{From: "a@b.c", To: []string{"d@e.f"}}, []byte("x"), time.Second); !errors.Is(err, ErrTemporary) {
		t.Errorf("unreachable: %v", err)
	}
	wrong := smtptest.Start(t, smtptest.Options{})
	err := Send(t.Context(), wrong.Config(), wrong.Client, smtptest.User, "wrong", Envelope{From: "me@example.org", To: []string{"a@x.org"}},
		[]byte("x\r\n"), 5*time.Second)
	if !errors.Is(err, ErrPermanent) || len(wrong.Messages()) != 0 {
		t.Errorf("wrong password: %v", err)
	}
}

func TestSendRefusesPlaintext(t *testing.T) {
	srv := smtptest.Start(t, smtptest.Options{NoStartTLS: true})
	if err := send(t, srv, []string{"a@example.org"}, []byte("x\r\n")); err == nil || len(srv.Messages()) != 0 {
		t.Errorf("sent over plaintext: %v", err)
	}
	cfg := srv.Config()
	cfg.TLS = "none"
	if err := Send(t.Context(), cfg, srv.Client, smtptest.User, smtptest.Password, Envelope{From: "me@example.org", To: []string{"a@x.org"}},
		[]byte("x\r\n"), 5*time.Second); err == nil {
		t.Error("an unknown tls mode was accepted")
	}
}

func TestSendRefusesUntrustedCertificate(t *testing.T) {
	srv := smtptest.Start(t, smtptest.Options{})
	err := Send(t.Context(), srv.Config(), nil, smtptest.User, smtptest.Password, // system roots: the test CA is unknown
		Envelope{From: "me@example.org", To: []string{"a@x.org"}}, []byte("x\r\n"), 5*time.Second)
	if err == nil || len(srv.Messages()) != 0 {
		t.Errorf("untrusted certificate: %v", err)
	}
}
