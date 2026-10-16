// SPDX-License-Identifier: AGPL-3.0-or-later

// Package smtptest runs an in-process SMTP server over real TLS for tests,
// recording what it receives. Only tests import it.
package smtptest

import (
	"bytes"
	"cmp"
	"crypto/tls"
	"encoding/pem"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/imaptest"
)

// Test credentials.
const (
	User     = "me@example.org"
	Password = "test-password"
)

// Options configure the server.
type Options struct {
	StartTLS   bool   // plain listener offering STARTTLS (default: implicit TLS)
	NoStartTLS bool   // plain listener without STARTTLS: clients must refuse it
	RejectRcpt string // this recipient is refused with 550
	FailData   int    // DATA fails with this SMTP code (4xx or 5xx)
	MaxSize    int64  // advertised SIZE limit
	DropData   bool   // accept DATA, then drop the connection before replying
	// User and Password are the accepted credentials (default: the
	// package's User and Password).
	User, Password string
}

// Received is one message the server accepted.
type Received struct {
	From string
	To   []string
	Data []byte
	Auth string // the user who logged in
}

// Server is a running test server.
type Server struct {
	Host   string
	Port   int
	Client *tls.Config // trusts the server's certificate
	CAPEM  []byte      // the certificate, PEM-encoded, to trust it next to others
	opts   Options

	mu       sync.Mutex
	received []Received
	failData atomic.Int64
	srv      *smtp.Server
	starttls bool
}

// Start runs a server until the test ends.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	cert, client := imaptest.Certificate(t)
	s := &Server{Host: "127.0.0.1", Client: client, opts: opts, starttls: opts.StartTLS || opts.NoStartTLS,
		CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})}
	s.failData.Store(int64(opts.FailData))
	s.srv = smtp.NewServer(backend{s})
	s.srv.Domain = "smtptest"
	s.srv.MaxMessageBytes = opts.MaxSize
	var ln net.Listener
	var err error
	switch {
	case opts.NoStartTLS:
		s.srv.AllowInsecureAuth = true // the client must still refuse
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	case opts.StartTLS:
		s.srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	default:
		ln, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	}
	if err != nil {
		t.Fatal(err)
	}
	s.Port = ln.Addr().(*net.TCPAddr).Port
	done := make(chan struct{})
	go func() { _ = s.srv.Serve(ln); close(done) }()
	// Closing the listener too: a test that ends before Serve starts would
	// otherwise wait for it forever.
	t.Cleanup(func() { s.Close(); _ = ln.Close(); <-done })
	return s
}

// Config returns the server as an account setting.
func (s *Server) Config() config.Server {
	mode := config.TLSImplicit
	if s.starttls {
		mode = config.TLSStartTLS
	}
	return config.Server{Host: s.Host, Port: s.Port, TLS: mode}
}

// Messages returns what the server accepted so far.
func (s *Server) Messages() []Received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Received(nil), s.received...)
}

// SetFailData makes DATA fail with code from now on (0: succeed).
func (s *Server) SetFailData(code int) { s.failData.Store(int64(code)) }

// Close stops the server (to test sending while offline).
func (s *Server) Close() { _ = s.srv.Close() }

type backend struct{ s *Server }

func (b backend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &session{s: b.s, conn: c}, nil
}

type session struct {
	s    *Server
	conn *smtp.Conn
	auth string
	msg  Received
}

func (ss *session) AuthMechanisms() []string { return []string{sasl.Plain} }

func (ss *session) Auth(string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(_, user, pass string) error {
		if user != cmp.Or(ss.s.opts.User, User) || pass != cmp.Or(ss.s.opts.Password, Password) {
			return smtp.ErrAuthFailed // 535, as real servers answer
		}
		ss.auth = user
		return nil
	}), nil
}

func (ss *session) Mail(from string, _ *smtp.MailOptions) error {
	if ss.auth == "" {
		return smtp.ErrAuthRequired
	}
	ss.msg = Received{From: from, Auth: ss.auth}
	return nil
}

func (ss *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	if to == ss.s.opts.RejectRcpt {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "no such user"}
	}
	ss.msg.To = append(ss.msg.To, to)
	return nil
}

func (ss *session) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if code := int(ss.s.failData.Load()); code != 0 {
		return &smtp.SMTPError{Code: code, Message: "failure " + strconv.Itoa(code)}
	}
	ss.msg.Data = bytes.Clone(data)
	ss.s.mu.Lock()
	ss.s.received = append(ss.s.received, ss.msg)
	ss.s.mu.Unlock()
	if ss.s.opts.DropData { // the message is taken, but the client never hears so
		_ = ss.conn.Conn().Close()
	}
	return nil
}

func (ss *session) Reset()        { ss.msg = Received{Auth: ss.auth} }
func (ss *session) Logout() error { return nil }
