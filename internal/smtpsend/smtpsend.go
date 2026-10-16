// SPDX-License-Identifier: AGPL-3.0-or-later

// Package smtpsend delivers outgoing mail over SMTP, with the same TLS rules
// as IMAP: implicit TLS or required STARTTLS, TLS 1.2 or later,
// certificates always verified, never plaintext. Only this package imports
// go-smtp.
package smtpsend

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"

	"github.com/pluja/nought/internal/config"
)

// Errors tell the outbox what to do next.
var (
	ErrTemporary = errors.New("temporary failure") // retry later: network, 4xx
	ErrPermanent = errors.New("permanent failure") // needs the user: 5xx, refused recipient, too large
	// ErrUnknown: the connection was lost after the whole message was
	// handed over, so it may have been delivered. Never retry by itself.
	ErrUnknown = errors.New("outcome unknown")
)

// Envelope is who the message is from and to (bare addresses). Bcc
// recipients are here, never in the message.
type Envelope struct {
	From string
	To   []string
}

// Send delivers raw to every recipient, or to none: recipients are offered
// one by one and the first refusal aborts before the message is sent. The
// greeting is "EHLO localhost", which names no host. timeout bounds the
// whole transaction.
func Send(ctx context.Context, srv config.Server, base *tls.Config, user, password string,
	env Envelope, raw []byte, timeout time.Duration) error {
	c, err := dial(ctx, srv, base, timeout)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()

	if max, ok := c.MaxMessageSize(); ok && max > 0 && len(raw) > max {
		return fmt.Errorf("the message is too large for the server (%d KB, the limit is %d KB): %w", len(raw)>>10, max>>10, ErrPermanent)
	}
	if err := c.Auth(sasl.NewPlainClient("", user, password)); err != nil {
		return classify("login", err)
	}
	if err := c.Mail(env.From, nil); err != nil {
		return classify("sender "+env.From, err)
	}
	for _, rcpt := range env.To {
		if err := c.Rcpt(rcpt, nil); err != nil {
			_ = c.Reset()
			return classify("recipient "+rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return classify("data", err)
	}
	if _, err := w.Write(raw); err != nil {
		return classify("data", err)
	}
	if err := w.Close(); err != nil {
		if _, ok := errors.AsType[*smtp.SMTPError](err); !ok {
			return fmt.Errorf("no answer after the message was sent (it may have been delivered): %w: %w", err, ErrUnknown)
		}
		return classify("data", err)
	}
	_ = c.Quit() // the message is accepted; a failed goodbye changes nothing
	return nil
}

func dial(ctx context.Context, srv config.Server, base *tls.Config, timeout time.Duration) (*smtp.Client, error) {
	tlsConfig := &tls.Config{}
	if base != nil {
		tlsConfig = base.Clone()
	}
	tlsConfig.ServerName, tlsConfig.MinVersion, tlsConfig.InsecureSkipVerify = srv.Host, tls.VersionTLS12, false
	addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w: %w", addr, err, ErrTemporary)
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	switch srv.TLS {
	case config.TLSImplicit:
		tc := tls.Client(conn, tlsConfig)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, tlsError(err)
		}
		return smtp.NewClient(tc), nil
	case config.TLSStartTLS:
		c, err := smtp.NewClientStartTLS(conn, tlsConfig) // fails if STARTTLS is not offered
		if err != nil {
			_ = conn.Close()
			return nil, tlsError(err)
		}
		return c, nil
	}
	_ = conn.Close()
	return nil, fmt.Errorf("tls mode %q is not allowed: %w", srv.TLS, ErrPermanent)
}

// tlsError: a certificate the system does not trust needs the user; other
// handshake trouble may pass.
func tlsError(err error) error {
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return fmt.Errorf("tls: %w: %w", err, ErrPermanent)
	}
	return fmt.Errorf("tls: %w: %w", err, ErrTemporary)
}

// classify maps an SMTP reply to ErrPermanent (5xx) or ErrTemporary (4xx,
// network trouble).
func classify(step string, err error) error {
	if se, ok := errors.AsType[*smtp.SMTPError](err); ok && se.Code >= 500 {
		return fmt.Errorf("%s: %w: %w", step, err, ErrPermanent)
	}
	return fmt.Errorf("%s: %w: %w", step, err, ErrTemporary)
}
