// SPDX-License-Identifier: AGPL-3.0-or-later

// Command smtpsink is the test environment's SMTP server: it accepts mail
// over implicit TLS on 127.0.0.1:10465, with the test certificate and the
// test password, and throws it away. It logs only that a message arrived,
// never its content. Development only: it refuses to run outside NOU_DEV=1.
package main

import (
	"crypto/tls"
	"errors"
	"io"
	"log"
	"os"
	"strings"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

const (
	addr     = "127.0.0.1:10465"
	password = "nought-test-password"
)

func main() {
	if os.Getenv("NOU_DEV") != "1" {
		log.Fatal("smtpsink: run via testenv/demo.sh (NOU_DEV=1)")
	}
	ca := os.Getenv("NOU_DEV_CA")
	cert, err := tls.LoadX509KeyPair(ca, strings.TrimSuffix(ca, ".crt")+".key")
	if err != nil {
		log.Fatalf("smtpsink: the test certificate (make testenv-up creates it): %v", err)
	}
	s := smtp.NewServer(backend{})
	s.Addr, s.Domain = addr, "localhost"
	s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	log.Printf("smtpsink: listening on %s", addr)
	log.Fatal(s.ListenAndServeTLS())
}

type backend struct{}

func (backend) NewSession(*smtp.Conn) (smtp.Session, error) { return &session{}, nil }

type session struct{ authed bool }

func (s *session) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *session) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, _, pass string) error {
		if pass != password {
			return smtp.ErrAuthFailed
		}
		s.authed = true
		return nil
	}), nil
}

func (s *session) Mail(string, *smtp.MailOptions) error {
	if !s.authed {
		return smtp.ErrAuthRequired
	}
	return nil
}

func (s *session) Rcpt(string, *smtp.RcptOptions) error { return nil }

func (s *session) Data(r io.Reader) error {
	n, err := io.Copy(io.Discard, r)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	log.Printf("smtpsink: accepted a message (%d bytes)", n)
	return nil
}

func (s *session) Reset()        {}
func (s *session) Logout() error { return nil }
