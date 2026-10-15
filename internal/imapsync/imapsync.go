// SPDX-License-Identifier: AGPL-3.0-or-later

// Package imapsync is nought's thin IMAP adapter: connect over TLS, list
// folders, fetch new messages, and expunge exact UIDs. It holds no sync
// logic (that lives in ops, where policy and verification are decided) and
// it is the only package that imports go-imap, which is still in beta.
package imapsync

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"nought/internal/config"
)

// Conn is an authenticated IMAP connection.
type Conn struct {
	c        *imapclient.Client
	stop     func() bool
	selected string
}

// dialTimeout bounds connecting and the TLS handshake.
const dialTimeout = 30 * time.Second

// Dial connects to srv with TLS and logs in. base may carry test roots; the
// server name and minimum version are always set here. Plaintext is never
// used: STARTTLS refuses servers that do not upgrade.
func Dial(ctx context.Context, srv config.Server, base *tls.Config, user, password string) (*Conn, error) {
	tlsConfig := &tls.Config{}
	if base != nil {
		tlsConfig = base.Clone()
	}
	tlsConfig.ServerName = srv.Host
	tlsConfig.MinVersion = tls.VersionTLS12
	opts := &imapclient.Options{
		TLSConfig: tlsConfig,
		Dialer:    &net.Dialer{Timeout: dialTimeout},
	}
	addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))

	var (
		c   *imapclient.Client
		err error
	)
	switch srv.TLS {
	case config.TLSImplicit:
		c, err = imapclient.DialTLS(addr, opts)
	case config.TLSStartTLS:
		c, err = imapclient.DialStartTLS(addr, opts)
	default:
		return nil, fmt.Errorf("unsupported TLS mode %q", srv.TLS)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	conn := &Conn{c: c}
	// go-imap has no context support: closing the connection unblocks any
	// pending command when ctx is cancelled.
	conn.stop = context.AfterFunc(ctx, func() { _ = c.Close() })

	if err := c.Login(user, password).Wait(); err != nil {
		return nil, errors.Join(fmt.Errorf("log in to %s: %w", addr, err), conn.Close())
	}
	return conn, nil
}

// Close logs out and closes the connection.
func (c *Conn) Close() error {
	c.stop()
	_ = c.c.Logout().Wait() // best effort; the server may already be gone
	return c.c.Close()
}

// Capabilities lists what the server supports, for diagnostics.
func (c *Conn) Capabilities() []string {
	var out []string
	for capability := range c.c.Caps() {
		out = append(out, string(capability))
	}
	slices.Sort(out)
	return out
}

// HasUIDPlus reports whether UID EXPUNGE is available, which is required to
// delete exact messages without touching anything else.
func (c *Conn) HasUIDPlus() bool { return c.c.Caps().Has(imap.CapUIDPlus) }

// Folder is a selectable server folder.
type Folder struct {
	Name string // hierarchy separated by "/" whatever the server uses
	Raw  string // the server's own name, used to select it
}

// Folders lists selectable folders.
func (c *Conn) Folders() ([]Folder, error) {
	list, err := c.c.List("", "*", nil).Collect()
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	var out []Folder
	for _, l := range list {
		if slices.Contains(l.Attrs, imap.MailboxAttrNoSelect) || slices.Contains(l.Attrs, imap.MailboxAttrNonExistent) {
			continue
		}
		name := l.Mailbox
		if l.Delim != 0 && l.Delim != '/' {
			name = strings.ReplaceAll(name, string(l.Delim), "/")
		}
		out = append(out, Folder{Name: name, Raw: l.Mailbox})
	}
	slices.SortFunc(out, func(a, b Folder) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Mailbox is the state of a selected folder.
type Mailbox struct {
	UIDValidity uint32
	Messages    uint32
}

// Select opens a folder read-write (needed for expunge).
func (c *Conn) Select(raw string) (Mailbox, error) {
	data, err := c.c.Select(raw, nil).Wait()
	if err != nil {
		return Mailbox{}, fmt.Errorf("select %s: %w", raw, err)
	}
	c.selected = raw
	return Mailbox{UIDValidity: data.UIDValidity, Messages: data.NumMessages}, nil
}

// MessageInfo is what is known about a message before fetching its body.
type MessageInfo struct {
	UID          uint32
	Size         int64
	InternalDate time.Time
	Seen         bool
	Flagged      bool
	Answered     bool
}

// List returns every message with a UID above after, in UID order.
func (c *Conn) List(after uint32) ([]MessageInfo, error) {
	if c.selected == "" {
		return nil, errors.New("no folder selected")
	}
	var set imap.UIDSet
	set.AddRange(imap.UID(after+1), 0) // after+1:*
	msgs, err := c.c.Fetch(set, &imap.FetchOptions{
		UID: true, Flags: true, InternalDate: true, RFC822Size: true,
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	var out []MessageInfo
	for _, m := range msgs {
		// "after+1:*" includes the last message even if its UID is lower.
		if uint32(m.UID) <= after {
			continue
		}
		out = append(out, MessageInfo{
			UID:          uint32(m.UID),
			Size:         m.RFC822Size,
			InternalDate: m.InternalDate.UTC(),
			Seen:         slices.Contains(m.Flags, imap.FlagSeen),
			Flagged:      slices.Contains(m.Flags, imap.FlagFlagged),
			Answered:     slices.Contains(m.Flags, imap.FlagAnswered),
		})
	}
	slices.SortFunc(out, func(a, b MessageInfo) int { return cmp.Compare(a.UID, b.UID) })
	return out, nil
}

// Fetch downloads one message without setting \Seen.
func (c *Conn) Fetch(uid uint32) ([]byte, error) {
	section := &imap.FetchItemBodySection{Peek: true}
	msgs, err := c.c.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{
		UID: true, BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch uid %d: %w", uid, err)
	}
	for _, m := range msgs {
		if uint32(m.UID) == uid {
			if raw := m.FindBodySection(section); raw != nil {
				return raw, nil
			}
		}
	}
	return nil, fmt.Errorf("fetch uid %d: %w", uid, ErrGone)
}

// ErrGone means the message disappeared from the server (another client
// deleted it).
var ErrGone = errors.New("message no longer on the server")

// ErrNoUIDPlus means the server cannot expunge individual messages.
var ErrNoUIDPlus = errors.New("server does not support UIDPLUS (UID EXPUNGE)")

// Expunge permanently deletes exactly these UIDs from the selected folder.
// It marks them \Deleted and runs UID EXPUNGE on the same set. A plain
// EXPUNGE is never used: it would also remove messages other clients marked
// for deletion.
func (c *Conn) Expunge(uids []uint32) error {
	if len(uids) == 0 {
		return nil
	}
	if !c.HasUIDPlus() {
		return ErrNoUIDPlus
	}
	var set imap.UIDSet
	for _, uid := range uids {
		set.AddNum(imap.UID(uid))
	}
	store := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}
	if err := c.c.Store(set, store, nil).Close(); err != nil {
		return fmt.Errorf("mark deleted: %w", err)
	}
	if err := c.c.UIDExpunge(set).Close(); err != nil {
		return fmt.Errorf("uid expunge: %w", err)
	}
	return nil
}
