// SPDX-License-Identifier: AGPL-3.0-or-later

// Package imapsync is nought's thin IMAP adapter: connect over TLS, list
// folders, fetch new messages, and expunge exact UIDs. It holds no sync
// logic (that lives in ops, where policy and verification are decided) and
// it is the only package that imports go-imap, which is still in beta.
//
// The server is not trusted: every socket read and write has a deadline,
// message bodies are size-limited while they are read, and listings are
// fetched in bounded chunks.
package imapsync

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/pluja/nought/internal/config"
)

// Timeouts. defaultIOTimeout bounds every single read or write, so a server
// that goes silent is detected, while a slow but progressing download is fine.
const (
	dialTimeout      = 30 * time.Second
	defaultIOTimeout = 2 * time.Minute
	logoutTimeout    = 5 * time.Second
)

// Limits on what a server may send.
const (
	MaxBatch   = 500   // UIDs per FETCH, STORE or EXPUNGE command
	maxFolders = 10000 // folders accepted from LIST
	maxNameLen = 1000  // bytes in a folder name
)

// Errors.
var (
	// ErrGone means the message disappeared from the server (another
	// client deleted it).
	ErrGone = errors.New("message no longer on the server")
	// ErrNoUIDPlus means the server cannot expunge individual messages.
	ErrNoUIDPlus = errors.New("server does not support UIDPLUS (UID EXPUNGE)")
	// ErrTooLarge means a message body exceeded the limit and was skipped.
	ErrTooLarge = errors.New("message larger than the limit")
)

// Conn is an authenticated IMAP connection.
type Conn struct {
	c        *imapclient.Client
	raw      *deadlineConn
	stop     func() bool
	done     <-chan struct{} // the dial context's: once closed, no command is sent
	selected string
}

// Dial connects to srv with TLS and logs in. base may carry test roots; the
// server name, minimum version and certificate verification are always set
// here. Plaintext is never used: STARTTLS refuses servers that do not
// upgrade. Cancelling ctx aborts the dial and closes the connection later.
//
// ioTimeout bounds each read and write on the connection; 0 means the
// default of two minutes.
func Dial(ctx context.Context, srv config.Server, base *tls.Config, user, password string, ioTimeout time.Duration) (*Conn, error) {
	tlsConfig := &tls.Config{}
	if base != nil {
		tlsConfig = base.Clone()
	}
	tlsConfig.ServerName = srv.Host
	tlsConfig.MinVersion = tls.VersionTLS12
	tlsConfig.InsecureSkipVerify = false
	opts := &imapclient.Options{TLSConfig: tlsConfig}
	addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	tcp, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	raw := &deadlineConn{Conn: tcp}
	raw.limit(ioTimeout)

	var c *imapclient.Client
	switch srv.TLS {
	case config.TLSImplicit:
		tlsConfig.NextProtos = []string{"imap"}
		tc := tls.Client(raw, tlsConfig)
		if err := tc.HandshakeContext(dialCtx); err != nil {
			_ = tcp.Close()
			return nil, fmt.Errorf("TLS with %s: %w", addr, err)
		}
		c = imapclient.New(tc, opts)
	case config.TLSStartTLS:
		c, err = imapclient.NewStartTLS(raw, opts) // refuses PREAUTH and failed upgrades
		if err != nil {
			_ = tcp.Close()
			return nil, fmt.Errorf("STARTTLS with %s: %w", addr, err)
		}
	default:
		_ = tcp.Close()
		return nil, fmt.Errorf("unsupported TLS mode %q", srv.TLS)
	}
	conn := &Conn{c: c, raw: raw, done: ctx.Done()}
	// go-imap has no context support: closing the connection unblocks any
	// pending command when ctx is cancelled.
	conn.stop = context.AfterFunc(ctx, func() { _ = c.Close() })

	if err := c.Login(user, password).Wait(); err != nil {
		return nil, errors.Join(fmt.Errorf("log in to %s: %w", addr, err), conn.Close())
	}
	return conn, nil
}

// Close logs out (briefly; the server may be gone) and closes the connection.
func (c *Conn) Close() error {
	c.raw.limit(logoutTimeout)
	_ = c.c.Logout().Wait()
	c.stop()
	return c.c.Close()
}

// live refuses to start a command once the context is cancelled. Closing
// the connection (on cancel) runs in another goroutine, so without this a
// command could still go out in between: an expunge after Ctrl-C.
func (c *Conn) live() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
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
	Name string // for display, tags and policies: "/"-separated, no control characters
	Raw  string // the server's own name, used to select it and to key sync state
	// Trash and Sent are set for the folders the server marks as its trash
	// and sent mail (RFC 6154 SPECIAL-USE); servers without SPECIAL-USE mark
	// none.
	Trash, Sent bool
	// Special is set for the folders the server marks as drafts, junk, all
	// mail or flagged: not mail of their own to archive (all mail and
	// flagged show messages that live elsewhere).
	Special bool
}

// Folders lists selectable folders.
func (c *Conn) Folders() ([]Folder, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	var opts *imap.ListOptions
	if c.c.Caps().Has(imap.CapSpecialUse) {
		opts = &imap.ListOptions{ReturnSpecialUse: true}
	}
	list, err := c.c.List("", "*", opts).Collect()
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	if len(list) > maxFolders {
		return nil, fmt.Errorf("server listed %d folders, more than the limit of %d", len(list), maxFolders)
	}
	var out []Folder
	for _, l := range list {
		if slices.Contains(l.Attrs, imap.MailboxAttrNoSelect) || slices.Contains(l.Attrs, imap.MailboxAttrNonExistent) ||
			len(l.Mailbox) > maxNameLen {
			continue
		}
		name := l.Mailbox
		if l.Delim != 0 && l.Delim != '/' {
			name = strings.ReplaceAll(name, string(l.Delim), "/")
		}
		name = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, name)
		out = append(out, Folder{Name: name, Raw: l.Mailbox,
			Trash: slices.Contains(l.Attrs, imap.MailboxAttrTrash), Sent: slices.Contains(l.Attrs, imap.MailboxAttrSent),
			Special: slices.ContainsFunc(l.Attrs, func(a imap.MailboxAttr) bool {
				return a == imap.MailboxAttrDrafts || a == imap.MailboxAttrJunk || a == imap.MailboxAttrAll || a == imap.MailboxAttrFlagged
			})})
	}
	slices.SortFunc(out, func(a, b Folder) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Exists reports whether a folder exists on the server, for one that is
// missing from the folder list: a server answering NO to STATUS has no
// such folder.
func (c *Conn) Exists(raw string) (bool, error) {
	if err := c.live(); err != nil {
		return false, err
	}
	_, err := c.c.Status(raw, &imap.StatusOptions{NumMessages: true}).Wait()
	if imapErr, ok := errors.AsType[*imap.Error](err); ok && imapErr.Type == imap.StatusResponseTypeNo {
		return false, nil
	}
	return err == nil, err
}

// Append stores msg in a folder (by its raw name) with the given flags
// ("Seen", "Flagged", "Answered") and internal date.
func (c *Conn) Append(raw string, flags []string, date time.Time, msg []byte) error {
	if err := c.live(); err != nil {
		return err
	}
	fl := make([]imap.Flag, 0, len(flags))
	for _, f := range flags {
		fl = append(fl, imap.Flag(`\`+f))
	}
	cmd := c.c.Append(raw, int64(len(msg)), &imap.AppendOptions{Flags: fl, Time: date})
	if _, err := cmd.Write(msg); err != nil {
		_ = cmd.Close()
		return fmt.Errorf("append to %s: %w", raw, err)
	}
	if err := cmd.Close(); err != nil {
		return fmt.Errorf("append to %s: %w", raw, err)
	}
	if _, err := cmd.Wait(); err != nil {
		return fmt.Errorf("append to %s: %w", raw, err)
	}
	return nil
}

// Mailbox is the state of a selected folder.
type Mailbox struct {
	UIDValidity uint32
	Messages    uint32
}

// Select opens a folder: read-write (needed for expunge), or read-only
// (EXAMINE), which changes nothing on the server.
func (c *Conn) Select(raw string, readOnly bool) (Mailbox, error) {
	if err := c.live(); err != nil {
		return Mailbox{}, err
	}
	c.selected = ""
	data, err := c.c.Select(raw, &imap.SelectOptions{ReadOnly: readOnly}).Wait()
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
	InternalDate time.Time // zero if the server sent none
	Seen         bool
	Flagged      bool
	Answered     bool
	Deleted      bool // marked \Deleted (by another client, or a failed expunge)
}

// UIDsAfter returns the UIDs above after, in order.
func (c *Conn) UIDsAfter(after uint32) ([]uint32, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	if c.selected == "" {
		return nil, errors.New("no folder selected")
	}
	var set imap.UIDSet
	set.AddRange(imap.UID(after+1), 0) // after+1:*
	data, err := c.c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{set}}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	var out []uint32
	for _, uid := range data.AllUIDs() {
		// "after+1:*" includes the last message even if its UID is lower.
		if uint32(uid) > after {
			out = append(out, uint32(uid))
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// ErrIncompleteList means the server listed fewer or more UIDs than the
// folder holds (a server problem, or mail arriving or leaving meanwhile).
var ErrIncompleteList = errors.New("the server's message list does not match the folder's message count")

// AllUIDs returns every UID in the selected folder, in order. The list is
// checked against the folder's message count, so a truncated answer is
// never taken for the folder's contents.
func (c *Conn) AllUIDs() ([]uint32, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	uids, err := c.UIDsAfter(0)
	if err != nil {
		return nil, err
	}
	if mb := c.c.Mailbox(); mb == nil || len(uids) != int(mb.NumMessages) {
		return nil, ErrIncompleteList
	}
	return uids, nil
}

// Lookup returns the current metadata of the given UIDs, fetched in
// batches; UIDs no longer on the server are absent from the result.
func (c *Conn) Lookup(uids []uint32) (map[uint32]MessageInfo, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	if c.selected == "" {
		return nil, errors.New("no folder selected")
	}
	out := make(map[uint32]MessageInfo, len(uids))
	for batch := range slices.Chunk(uids, MaxBatch) {
		msgs, err := c.c.Fetch(uidSet(batch), &imap.FetchOptions{
			UID: true, Flags: true, InternalDate: true, RFC822Size: true,
		}).Collect()
		if err != nil {
			return nil, fmt.Errorf("look up messages: %w", err)
		}
		for _, m := range msgs {
			if !slices.Contains(batch, uint32(m.UID)) {
				continue // unrequested data from the server
			}
			out[uint32(m.UID)] = MessageInfo{
				UID:          uint32(m.UID),
				Size:         m.RFC822Size,
				InternalDate: m.InternalDate.UTC(),
				Seen:         hasFlag(m.Flags, imap.FlagSeen),
				Flagged:      hasFlag(m.Flags, imap.FlagFlagged),
				Answered:     hasFlag(m.Flags, imap.FlagAnswered),
				Deleted:      hasFlag(m.Flags, imap.FlagDeleted),
			}
		}
	}
	return out, nil
}

// Infos returns the metadata of uids in UID order, skipping vanished ones.
func (c *Conn) Infos(uids []uint32) ([]MessageInfo, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	found, err := c.Lookup(uids)
	if err != nil {
		return nil, err
	}
	out := make([]MessageInfo, 0, len(found))
	for _, m := range found {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b MessageInfo) int { return cmp.Compare(a.UID, b.UID) })
	return out, nil
}

// IMAP flags are case-insensitive.
func hasFlag(flags []imap.Flag, want imap.Flag) bool {
	return slices.ContainsFunc(flags, func(f imap.Flag) bool { return strings.EqualFold(string(f), string(want)) })
}

func uidSet(uids []uint32) imap.UIDSet {
	var set imap.UIDSet
	for _, uid := range uids {
		set.AddNum(imap.UID(uid))
	}
	return set
}

// Fetch downloads one message without setting \Seen, keeping at most max
// bytes in memory. A larger body is read and discarded (the size the server
// announced is not trusted) and ErrTooLarge is returned; the connection
// stays usable.
func (c *Conn) Fetch(uid uint32, limit int64) ([]byte, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	if c.selected == "" {
		return nil, errors.New("no folder selected")
	}
	section := &imap.FetchItemBodySection{Peek: true}
	cmd := c.c.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{
		UID: true, BodySection: []*imap.FetchItemBodySection{section},
	})
	var body []byte
	found, tooLarge := false, false
	for msg := cmd.Next(); msg != nil; msg = cmd.Next() {
		var msgUID uint32
		var msgBody []byte
		for item := msg.Next(); item != nil; item = msg.Next() {
			switch item := item.(type) {
			case imapclient.FetchItemDataUID:
				msgUID = uint32(item.UID)
			case imapclient.FetchItemDataBodySection:
				b, err := io.ReadAll(io.LimitReader(item.Literal, limit+1))
				if err != nil {
					return nil, fmt.Errorf("fetch uid %d: %w", uid, err)
				}
				if int64(len(b)) > limit {
					// Drain the rest so the protocol stays in step; the
					// deadlines bound how long a hostile server can stream.
					if _, err := io.Copy(io.Discard, item.Literal); err != nil {
						return nil, fmt.Errorf("fetch uid %d: %w", uid, err)
					}
					tooLarge = true
					continue
				}
				msgBody = b
			}
		}
		if msgUID == uid && msgBody != nil {
			body, found = msgBody, true
		}
	}
	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("fetch uid %d: %w", uid, err)
	}
	if tooLarge {
		return nil, fmt.Errorf("uid %d: %w", uid, ErrTooLarge)
	}
	if !found {
		return nil, fmt.Errorf("fetch uid %d: %w", uid, ErrGone)
	}
	return body, nil
}

// Expunge permanently deletes exactly these UIDs from the selected folder.
// It marks them \Deleted and runs UID EXPUNGE on the same set. A plain
// EXPUNGE is never used: it would also remove messages other clients marked
// for deletion. If UID EXPUNGE fails, the \Deleted flag is removed again
// from the messages that did not carry it before, so no other client
// deletes them later. Callers pass at most MaxBatch UIDs.
func (c *Conn) Expunge(uids []uint32) error {
	if err := c.live(); err != nil {
		return err
	}
	if len(uids) == 0 {
		return nil
	}
	if len(uids) > MaxBatch {
		return fmt.Errorf("expunge: %d UIDs, more than %d", len(uids), MaxBatch)
	}
	if !c.HasUIDPlus() {
		return ErrNoUIDPlus
	}
	before, err := c.Lookup(uids)
	if err != nil {
		return err
	}
	var ours []uint32 // messages we are about to flag
	for _, uid := range uids {
		if m, ok := before[uid]; ok && !m.Deleted {
			ours = append(ours, uid)
		}
	}
	set := uidSet(uids)
	mark := &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}
	if err := c.c.Store(set, mark, nil).Close(); err != nil {
		return fmt.Errorf("mark deleted: %w", err)
	}
	if err := c.c.UIDExpunge(set).Close(); err != nil {
		if len(ours) > 0 {
			unmark := &imap.StoreFlags{Op: imap.StoreFlagsDel, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}
			_ = c.c.Store(uidSet(ours), unmark, nil).Close() // best effort; the connection may be gone
		}
		return fmt.Errorf("uid expunge: %w", err)
	}
	return nil
}

// deadlineConn sets a fresh deadline before every read and write, so a
// silent server fails the current command instead of hanging forever.
type deadlineConn struct {
	net.Conn
	timeout atomic.Int64 // nanoseconds; 0 means defaultIOTimeout
}

func (d *deadlineConn) Read(p []byte) (int, error) {
	if err := d.SetReadDeadline(time.Now().Add(d.current())); err != nil { //nolint:forbidigo // a network deadline is wall-clock time
		return 0, err
	}
	return d.Conn.Read(p)
}

func (d *deadlineConn) Write(p []byte) (int, error) {
	if err := d.SetWriteDeadline(time.Now().Add(d.current())); err != nil { //nolint:forbidigo // a network deadline is wall-clock time
		return 0, err
	}
	return d.Conn.Write(p)
}

// limit sets the timeout, e.g. shorter for a courtesy logout.
func (d *deadlineConn) limit(t time.Duration) { d.timeout.Store(int64(t)) }

func (d *deadlineConn) current() time.Duration {
	if t := time.Duration(d.timeout.Load()); t > 0 {
		return t
	}
	return defaultIOTimeout
}

// maxHeader bounds a header block fetched for verification.
const maxHeader = 1 << 20

// Headers fetches the header block (BODY.PEEK[HEADER], without setting
// \Seen) of each UID. UIDs no longer on the server, or with headers larger
// than the limit, are absent from the result.
func (c *Conn) Headers(uids []uint32) (map[uint32][]byte, error) {
	if err := c.live(); err != nil {
		return nil, err
	}
	if c.selected == "" {
		return nil, errors.New("no folder selected")
	}
	out := make(map[uint32][]byte, len(uids))
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	for batch := range slices.Chunk(uids, MaxBatch) {
		cmd := c.c.Fetch(uidSet(batch), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}})
		for msg := cmd.Next(); msg != nil; msg = cmd.Next() {
			var uid uint32
			var header []byte
			for item := msg.Next(); item != nil; item = msg.Next() {
				switch item := item.(type) {
				case imapclient.FetchItemDataUID:
					uid = uint32(item.UID)
				case imapclient.FetchItemDataBodySection:
					b, err := io.ReadAll(io.LimitReader(item.Literal, maxHeader+1))
					if err != nil {
						return nil, errors.Join(fmt.Errorf("fetch headers: %w", err), cmd.Close())
					}
					if len(b) > maxHeader {
						if _, err := io.Copy(io.Discard, item.Literal); err != nil {
							return nil, errors.Join(fmt.Errorf("fetch headers: %w", err), cmd.Close())
						}
						b = nil
					}
					header = b
				}
			}
			if header != nil && slices.Contains(batch, uid) {
				out[uid] = header
			}
		}
		if err := cmd.Close(); err != nil {
			return nil, fmt.Errorf("fetch headers: %w", err)
		}
	}
	return out, nil
}
