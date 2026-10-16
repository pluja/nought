// SPDX-License-Identifier: AGPL-3.0-or-later

// Package imaptest runs an in-memory IMAP server over real TLS for tests,
// with helpers to add messages, change UIDVALIDITY and break connections.
// Only tests import it.
package imaptest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/pluja/nought/internal/config"
)

// Test credentials.
const (
	User     = "me@example.org"
	Password = "test-password"
)

// Options configure the server.
type Options struct {
	NoUIDPlus bool // advertise only IMAP4rev1, without UIDPLUS
}

// Server is a running test server.
type Server struct {
	Host   string
	Port   int
	Client *tls.Config // trusts the server's certificate
	CAPEM  []byte      // the certificate, PEM-encoded, for other processes

	user      *imapmemserver.User
	breakNext atomic.Int64 // >0: the next connection dies after this many bytes
	wg        sync.WaitGroup
}

// Start runs a server until the test ends. It has INBOX, Sent and Trash.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	cert, caPEM, pool := selfSigned(t)
	mem := imapmemserver.New()
	user := imapmemserver.NewUser(User, Password)
	for _, mbox := range []string{"INBOX", "Sent", "Trash"} {
		if err := user.Create(mbox, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)

	caps := imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapUIDPlus: {}}
	if opts.NoUIDPlus {
		caps = imap.CapSet{imap.CapIMAP4rev1: {}}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		Caps:      caps,
		// The listener is TLS, but the fault-injecting wrapper hides that
		// from the server, which would otherwise refuse LOGIN.
		InsecureAuth: true,
	})
	inner, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		Host:   "127.0.0.1",
		Port:   inner.Addr().(*net.TCPAddr).Port,
		Client: &tls.Config{RootCAs: pool},
		CAPEM:  caPEM,
		user:   user,
	}
	ln := &breakingListener{Listener: inner, s: s}
	s.wg.Go(func() { _ = srv.Serve(ln) })
	t.Cleanup(func() {
		_ = srv.Close()
		s.wg.Wait()
	})
	return s
}

// Config returns the server as an account server setting.
func (s *Server) Config() config.Server {
	return config.Server{Host: s.Host, Port: s.Port, TLS: config.TLSImplicit}
}

// BreakNextConnection makes the next accepted connection drop after the
// server has written n bytes, to simulate network failures mid-sync.
func (s *Server) BreakNextConnection(n int64) { s.breakNext.Store(n) }

func (s *Server) client(t testing.TB) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialTLS(net.JoinHostPort(s.Host, strconv.Itoa(s.Port)), &imapclient.Options{TLSConfig: s.Client})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(User, Password).Wait(); err != nil {
		t.Fatal(err)
	}
	return c
}

// Append adds a message and returns its UID.
func (s *Server) Append(t testing.TB, mailbox, raw string, date time.Time, flags ...imap.Flag) uint32 {
	t.Helper()
	c := s.client(t)
	defer func() { _ = c.Close() }()
	cmd := c.Append(mailbox, int64(len(raw)), &imap.AppendOptions{Flags: flags, Time: date})
	if _, err := cmd.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := cmd.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return uint32(data.UID)
}

// UIDs lists the UIDs in a mailbox.
func (s *Server) UIDs(t testing.TB, mailbox string) []uint32 {
	t.Helper()
	c := s.client(t)
	defer func() { _ = c.Close() }()
	if _, err := c.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	var out []uint32
	for _, uid := range data.AllUIDs() {
		out = append(out, uint32(uid))
	}
	return out
}

// SetDeleted marks a message \Deleted, as another mail client might.
func (s *Server) SetDeleted(t testing.TB, mailbox string, uid uint32) {
	t.Helper()
	c := s.selected(t, mailbox)
	defer func() { _ = c.Close() }()
	store := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}
	if err := c.Store(imap.UIDSetNum(imap.UID(uid)), store, nil).Close(); err != nil {
		t.Fatal(err)
	}
}

// Recreate deletes and recreates a mailbox (empty), changing its
// UIDVALIDITY, as a server migration might.
func (s *Server) Recreate(t testing.TB, mailbox string) {
	t.Helper()
	if err := s.user.Delete(mailbox); err != nil {
		t.Fatal(err)
	}
	if err := s.user.Create(mailbox, nil); err != nil {
		t.Fatal(err)
	}
}

// CreateMailbox adds a mailbox.
func (s *Server) CreateMailbox(t testing.TB, name string) {
	t.Helper()
	if err := s.user.Create(name, nil); err != nil {
		t.Fatal(err)
	}
}

type breakingListener struct {
	net.Listener
	s *Server
}

func (l *breakingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if n := l.s.breakNext.Swap(0); n > 0 {
		return &breakingConn{Conn: c, left: n}, nil
	}
	return c, nil
}

type breakingConn struct {
	net.Conn
	left int64
}

func (c *breakingConn) Write(p []byte) (int, error) {
	if int64(len(p)) >= c.left {
		_ = c.Close()
		return 0, net.ErrClosed
	}
	c.left -= int64(len(p))
	return c.Conn.Write(p)
}

// Certificate returns a fresh self-signed server certificate for 127.0.0.1
// and a client configuration that trusts it, for hand-written fake servers.
func Certificate(t testing.TB) (tls.Certificate, *tls.Config) {
	t.Helper()
	cert, _, pool := selfSigned(t)
	return cert, &tls.Config{RootCAs: pool}
}

func selfSigned(t testing.TB) (tls.Certificate, []byte, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "nought test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, certPEM, pool
}

// Move moves a message to another mailbox, as a phone's "archive" does
// (COPY, then mark deleted and expunge: works without the MOVE extension).
func (s *Server) Move(t testing.TB, from string, uid uint32, to string) {
	t.Helper()
	s.Copy(t, from, uid, to)
	s.Expunge(t, from, uid)
}

// Copy copies a message to another mailbox.
func (s *Server) Copy(t testing.TB, from string, uid uint32, to string) {
	t.Helper()
	c := s.selected(t, from)
	defer func() { _ = c.Close() }()
	if _, err := c.Copy(imap.UIDSetNum(imap.UID(uid)), to).Wait(); err != nil {
		t.Fatal(err)
	}
}

// Expunge deletes a message for good, as a phone's "delete" may. Other
// messages already marked deleted in the mailbox go too.
func (s *Server) Expunge(t testing.TB, mailbox string, uid uint32) {
	t.Helper()
	c := s.selected(t, mailbox)
	defer func() { _ = c.Close() }()
	mark := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}
	if err := c.Store(imap.UIDSetNum(imap.UID(uid)), mark, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge().Close(); err != nil {
		t.Fatal(err)
	}
}

// DeleteMailbox removes a mailbox with its messages.
func (s *Server) DeleteMailbox(t testing.TB, name string) {
	t.Helper()
	if err := s.user.Delete(name); err != nil {
		t.Fatal(err)
	}
}

// selected returns a client with mailbox selected read-write.
func (s *Server) selected(t testing.TB, mailbox string) *imapclient.Client {
	t.Helper()
	c := s.client(t)
	if _, err := c.Select(mailbox, nil).Wait(); err != nil {
		_ = c.Close()
		t.Fatal(err)
	}
	return c
}
