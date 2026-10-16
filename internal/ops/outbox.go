// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	netmail "net/mail"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pluja/nought/internal/atomicfile"
	"github.com/pluja/nought/internal/compose"
	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/imapsync"
	"github.com/pluja/nought/internal/mailmsg"
	"github.com/pluja/nought/internal/secret"
	"github.com/pluja/nought/internal/smtpsend"
)

// The outbox holds built messages until they are sent: <id>.eml (the
// bytes sent, without Bcc), <id>.stored.eml (the copy archived, with Bcc)
// and <id>.json (the entry). Sending is never repeated and nothing is
// lost: an entry is saved as "sending" before SMTP starts, as "sent" once
// SMTP accepted it, and removed only after the local copy is archived.

// Outbox entry states.
const (
	StateWaiting = "waiting" // to be sent at SendAfter (or retried)
	StateSending = "sending" // nought stopped while sending: it may have gone out
	StateFailed  = "failed"  // refused; needs the user
	StateSent    = "sent"    // sent; the local copy is still to be archived
)

// defaultSMTPTimeout bounds one SMTP transaction.
const defaultSMTPTimeout = 2 * time.Minute

// OutboxEntry is a message waiting to be sent.
type OutboxEntry struct {
	ID        string    `json:"id"`
	Account   string    `json:"account"`
	DraftID   string    `json:"draft_id"`
	From      string    `json:"from"` // envelope sender
	To        []string  `json:"to"`   // envelope recipients, Bcc included
	Subject   string    `json:"subject"`
	MessageID string    `json:"message_id"`
	SendAfter time.Time `json:"send_after"`
	State     string    `json:"state"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
}

// SendIn sends a draft. Now skips the undo window (the CLI).
type SendIn struct {
	DraftID string `json:"draft_id"`
	Now     bool   `json:"now,omitempty"`
}

// MsgSend builds a draft into an outbox entry, sent after the undo window.
var MsgSend = Op[SendIn, OutboxEntry]{
	Name: "msg.send", Summary: "Send a draft (after the undo window)", Perm: Send,
	Run: runSend,
}

func runSend(ctx context.Context, env *Env, in SendIn) (OutboxEntry, error) {
	info, d, err := loadDraft(env, in.DraftID)
	if err != nil {
		return OutboxEntry{}, err
	}
	a, err := account(env, info.Account)
	if err != nil {
		return OutboxEntry{}, err
	}
	if a.SMTP == (config.Server{}) {
		return OutboxEntry{}, usageError("account %s has no SMTP server: add one with nou account set %s --smtp-host … --smtp-port … --smtp-tls …",
			a.Name, a.Name)
	}
	var blocking []string
	for _, p := range draftChecks(ctx, env, info, d) {
		if p.Blocking {
			blocking = append(blocking, p.Text)
		}
	}
	if len(blocking) > 0 {
		return OutboxEntry{}, usageError("cannot send: %s", strings.Join(blocking, "; "))
	}
	out, err := buildOutgoing(ctx, env, info, d)
	if err != nil {
		return OutboxEntry{}, err
	}
	stored, sent, msgID, err := buildBoth(out, env.Now())
	if err != nil {
		return OutboxEntry{}, err
	}
	id, err := newID()
	if err != nil {
		return OutboxEntry{}, err
	}
	e := OutboxEntry{ID: id, Account: a.Name, DraftID: info.ID, From: bare(d.From), Subject: d.Subject,
		MessageID: msgID, SendAfter: env.Now(), State: StateWaiting}
	for _, r := range slices.Concat(d.To, d.Cc, d.Bcc) {
		e.To = append(e.To, bare(r))
	}
	if !in.Now {
		cfg, err := env.Config()
		if err != nil {
			return OutboxEntry{}, err
		}
		e.SendAfter = env.Now().Add(cfg.SendDelayDuration())
	}
	// The bytes first, the entry last: an entry always has its message.
	for ext, data := range map[string][]byte{".eml": sent, ".stored.eml": stored} {
		if err := atomicfile.Write(entryPath(env, id, ext), 0o600, func(w io.Writer) error {
			_, err := w.Write(data)
			return err
		}); err != nil {
			return OutboxEntry{}, fmt.Errorf("write outbox: %w", err)
		}
	}
	return e, saveEntry(env, e)
}

// buildOutgoing turns a draft into a message to build: attachments read
// from disk, plus the original's when forwarding.
func buildOutgoing(ctx context.Context, env *Env, info DraftInfo, d compose.Draft) (mailmsg.Outgoing, error) {
	out := mailmsg.Outgoing{From: d.From, To: d.To, Cc: d.Cc, Bcc: d.Bcc, Subject: d.Subject,
		InReplyTo: d.InReplyTo, References: d.References, Body: d.Body}
	for _, p := range d.Attach {
		data, err := os.ReadFile(expandPath(p))
		if err != nil {
			return out, usageError("cannot attach %s: %v", p, err)
		}
		ct := mime.TypeByExtension(filepath.Ext(p))
		if ct == "" {
			ct = "application/octet-stream"
		}
		out.Attachments = append(out.Attachments, mailmsg.OutgoingAttachment{Filename: filepath.Base(p), ContentType: ct, Data: data})
	}
	if info.Kind == KindForward {
		_, raw, _, err := source(ctx, env, info.Account, info.SourceHash)
		if err != nil {
			return out, err
		}
		carried, err := mailmsg.AttachmentData(raw)
		if err != nil {
			return out, err
		}
		out.Attachments = append(out.Attachments, carried...)
	}
	return out, nil
}

// buildBoth builds the stored copy (with Bcc) and the bytes sent (without),
// with the same date and Message-ID.
func buildBoth(m mailmsg.Outgoing, now time.Time) (stored, sent []byte, id string, err error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, nil, "", err
	}
	m.IncludeBcc = true
	if stored, id, err = mailmsg.Build(m, now, bytes.NewReader(random)); err != nil {
		return nil, nil, "", usageError("%v", err)
	}
	m.IncludeBcc = false
	sent, _, err = mailmsg.Build(m, now, bytes.NewReader(random))
	return stored, sent, id, err
}

// bare is the address of "Name <addr>".
func bare(s string) string {
	if a, err := netmail.ParseAddress(s); err == nil {
		return a.Address
	}
	return strings.TrimSpace(s)
}

// OutboxCancel takes an entry back to its draft: the undo of a send, or
// the way to edit one that failed. It is too late once the server accepted
// the message.
var OutboxCancel = Op[DraftIDIn, DraftOut]{
	Name: "outbox.cancel", Summary: "Take a message out of the outbox, back to its draft", Perm: Draft,
	Run: func(ctx context.Context, env *Env, in DraftIDIn) (DraftOut, error) {
		env.outboxMu.Lock()
		defer env.outboxMu.Unlock()
		e, err := loadEntry(env, in.ID)
		if err != nil {
			return DraftOut{}, err
		}
		if e.State == StateSent {
			return DraftOut{}, usageError("%q was already sent", e.Subject)
		}
		if err := removeEntry(env, e.ID); err != nil {
			return DraftOut{}, err
		}
		info, d, err := loadDraft(env, e.DraftID)
		if err != nil {
			return DraftOut{}, err
		}
		if e.LastError != "" {
			info.LastError = e.LastError
			if err := saveDraftInfo(env, info); err != nil {
				return DraftOut{}, err
			}
		}
		return draftOut(ctx, env, info, d), nil
	},
}

// OutboxRetry sends a failed entry (or one that may have been sent) again
// at the next flush.
var OutboxRetry = Op[DraftIDIn, OutboxEntry]{
	Name: "outbox.retry", Summary: "Try sending a message again", Perm: Send,
	Run: func(_ context.Context, env *Env, in DraftIDIn) (OutboxEntry, error) {
		env.outboxMu.Lock()
		defer env.outboxMu.Unlock()
		e, err := loadEntry(env, in.ID)
		if err != nil {
			return e, err
		}
		if e.State != StateFailed && e.State != StateSending {
			return e, usageError("%q is not waiting for a retry", e.Subject)
		}
		e.State, e.SendAfter = StateWaiting, env.Now()
		return e, saveEntry(env, e)
	},
}

// OutboxList lists the outbox, the soonest first.
var OutboxList = Op[struct{}, []OutboxEntry]{
	Name: "outbox.list", Summary: "List messages waiting to be sent", Perm: Read,
	Run: func(_ context.Context, env *Env, _ struct{}) ([]OutboxEntry, error) {
		return listEntries(env)
	},
}

// FlushIn sends what is due; All also sends what is still in its undo
// window (quitting ends the window).
type FlushIn struct {
	All bool `json:"all,omitempty"`
}

// FlushOut reports a flush.
type FlushOut struct {
	Sent    int      `json:"sent"`
	Failed  int      `json:"failed"`  // refused in this flush
	Waiting int      `json:"waiting"` // still waiting (undo window, or a retry)
	Notes   []string `json:"notes,omitempty"`
}

// OutboxFlush sends the due messages and archives what was sent.
var OutboxFlush = Op[FlushIn, FlushOut]{
	Name: "outbox.flush", Summary: "Send the messages that are due", Perm: Send,
	Run: func(ctx context.Context, env *Env, in FlushIn) (FlushOut, error) {
		env.outboxMu.Lock()
		defer env.outboxMu.Unlock()
		entries, err := listEntries(env)
		if err != nil {
			return FlushOut{}, err
		}
		var out FlushOut
		var errs []error
		for _, e := range entries {
			if e.State == StateWaiting && !in.All && e.SendAfter.After(env.Now()) {
				out.Waiting++
				continue
			}
			if err := flushOne(ctx, env, e, &out); err != nil {
				errs = append(errs, fmt.Errorf("%q: %w", e.Subject, err))
			}
		}
		return out, errors.Join(errs...)
	},
}

// flushOne sends one due entry, or finishes its bookkeeping.
func flushOne(ctx context.Context, env *Env, e OutboxEntry, out *FlushOut) error {
	switch e.State {
	case StateSending:
		out.Notes = append(out.Notes, fmt.Sprintf("%q may have been sent (nought stopped while sending it): retry or discard it", e.Subject))
		return nil
	case StateFailed:
		return nil
	case StateSent:
		return archiveSent(ctx, env, e, out)
	}
	e.State, e.Attempts = StateSending, e.Attempts+1
	if err := saveEntry(env, e); err != nil {
		return err
	}
	err := deliver(ctx, env, e)
	switch {
	case err == nil:
		e.State, e.LastError = StateSent, ""
		if err := saveEntry(env, e); err != nil {
			return err
		}
		out.Sent++
		return archiveSent(ctx, env, e, out)
	case errors.Is(err, smtpsend.ErrPermanent):
		e.State, e.LastError = StateFailed, err.Error()
		out.Failed++
	default: // temporary: offline, or the server asked to try later
		e.State, e.LastError = StateWaiting, err.Error()
		out.Waiting++
	}
	return saveEntry(env, e)
}

func deliver(ctx context.Context, env *Env, e OutboxEntry) error {
	a, err := account(env, e.Account)
	if err != nil {
		return err
	}
	password, err := secret.Password(ctx, env.Secrets, a.Name, a.PasswordCommand)
	if err != nil {
		return fmt.Errorf("%w: %w", err, smtpsend.ErrTemporary)
	}
	raw, err := os.ReadFile(entryPath(env, e.ID, ".eml"))
	if err != nil {
		return err
	}
	timeout := cmpOr(env.SMTPTimeout, defaultSMTPTimeout)
	return smtpsend.Send(ctx, a.SMTP, env.TLS, a.Login(), password,
		smtpsend.Envelope{From: e.From, To: e.To}, raw, timeout)
}

func cmpOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// archiveSent keeps the sent message locally (read, out of the inbox),
// marks the answered message replied, appends the copy to the server's
// Sent folder if the account asks for it, and clears the draft and entry.
func archiveSent(ctx context.Context, env *Env, e OutboxEntry, out *FlushOut) error {
	stored, err := os.ReadFile(entryPath(env, e.ID, ".stored.eml"))
	if err != nil {
		return err
	}
	if _, _, err := archive(ctx, env, e.Account, stored, "S", []string{folderTag("Sent")}); err != nil {
		return fmt.Errorf("archive the sent copy: %w", err)
	}
	if info, err := loadDraftInfo(env, e.DraftID); err == nil && (info.Kind == KindReply || info.Kind == KindReplyAll) {
		if _, err := runTag(ctx, env, TagIn{Query: "hash:" + info.SourceHash + " account:" + e.Account, Add: []string{TagReplied}}); err != nil {
			out.Notes = append(out.Notes, "could not mark the original replied: "+err.Error())
		}
	}
	if a, err := account(env, e.Account); err == nil && a.SaveSent == config.SaveSentServer {
		if err := appendSent(ctx, env, a, stored); err != nil {
			out.Notes = append(out.Notes, fmt.Sprintf("%q was sent, but not copied to the server's Sent folder: %v", e.Subject, err))
		}
	}
	if err := writeTagsDump(ctx, env); err != nil {
		return err
	}
	return errors.Join(removeDraft(env, e.DraftID), removeEntry(env, e.ID))
}

// appendSent copies a sent message to the server's Sent folder (marked
// \Sent, else by name).
func appendSent(ctx context.Context, env *Env, a *config.Account, raw []byte) error {
	password, err := secret.Password(ctx, env.Secrets, a.Name, a.PasswordCommand)
	if err != nil {
		return err
	}
	conn, err := imapsync.Dial(ctx, a.IMAP, env.TLS, a.Login(), password, env.IMAPTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	folders, err := conn.Folders()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(folders, func(f imapsync.Folder) bool { return f.Sent })
	if i < 0 {
		i = slices.IndexFunc(folders, func(f imapsync.Folder) bool {
			switch strings.ToLower(path.Base(f.Name)) {
			case "sent", "sent items", "sent messages", "sent mail":
				return true
			}
			return false
		})
	}
	if i < 0 {
		return errors.New("the server has no Sent folder")
	}
	return conn.Append(folders[i].Raw, []string{"Seen"}, env.Now(), raw)
}

func entryPath(env *Env, id, ext string) string { return filepath.Join(env.Paths.OutboxDir, id+ext) }

func saveEntry(env *Env, e OutboxEntry) error {
	return atomicfile.Write(entryPath(env, e.ID, ".json"), 0o600, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(e)
	})
}

func loadEntry(env *Env, id string) (OutboxEntry, error) {
	if !validID(id) {
		return OutboxEntry{}, usageError("%q is not an outbox id", id)
	}
	b, err := os.ReadFile(entryPath(env, id, ".json"))
	if errors.Is(err, os.ErrNotExist) {
		return OutboxEntry{}, fmt.Errorf("outbox entry %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return OutboxEntry{}, err
	}
	var e OutboxEntry
	if err := json.Unmarshal(b, &e); err != nil {
		return OutboxEntry{}, fmt.Errorf("outbox entry %s: %w", id, err)
	}
	e.ID = id
	return e, nil
}

func listEntries(env *Env) ([]OutboxEntry, error) {
	files, err := os.ReadDir(env.Paths.OutboxDir)
	if errors.Is(err, os.ErrNotExist) {
		return []OutboxEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []OutboxEntry{}
	for _, f := range files {
		id, ok := strings.CutSuffix(f.Name(), ".json")
		if !ok || !validID(id) {
			continue
		}
		e, err := loadEntry(env, id)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(x, y OutboxEntry) int { return x.SendAfter.Compare(y.SendAfter) })
	return out, nil
}

func removeEntry(env *Env, id string) error {
	var errs []error
	for _, ext := range []string{".json", ".eml", ".stored.eml"} { // the entry first: no entry without its bytes
		if err := os.Remove(entryPath(env, id, ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
