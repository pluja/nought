// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"nought/internal/config"
	"nought/internal/index"
	"nought/internal/mailmsg"
	"nought/internal/query"
	"nought/internal/render"
	"nought/internal/store"
)

// IDLen is how many hex characters of the content hash nou shows as a
// message ID. Any unique prefix of at least index.MinPrefix is accepted.
const IDLen = 12

// MessageOut is a message as shown to users and agents. Header fields are
// untrusted content from the sender.
type MessageOut struct {
	ID        string     `json:"id"`
	Account   string     `json:"account"`
	Date      *time.Time `json:"date,omitempty"`
	From      string     `json:"from"`
	To        []string   `json:"to,omitempty"`
	Subject   string     `json:"subject"`
	Tags      []string   `json:"tags"`
	MessageID string     `json:"message_id,omitempty"`
	Thread    string     `json:"thread,omitempty"`
	Size      int64      `json:"size"`
	// Server lists where the message still is on a mail server. Empty
	// means the message exists only in the local archive.
	Server []ServerLocation `json:"server"`
}

// ServerLocation is a copy of a message still on a mail server.
type ServerLocation struct {
	Folder string `json:"folder"`
	// LeavesAt is when sync will delete it from the server, if deletion is
	// enabled for the account; nil if it stays.
	LeavesAt *time.Time `json:"leaves_at,omitempty"`
}

func messageOut(m index.Message) MessageOut {
	out := MessageOut{
		ID: m.Hash[:IDLen], Account: m.Account, From: m.From, To: m.To, Subject: m.Subject,
		Tags: m.Tags, MessageID: m.MessageID, Thread: m.ThreadKey, Size: m.Size, Server: []ServerLocation{},
	}
	if !m.Date.IsZero() {
		d := m.Date
		out.Date = &d
	}
	return out
}

// SearchIn is a query with paging.
type SearchIn struct {
	Query  string `json:"query"`
	Limit  int    `json:"limit,omitempty"` // default 50, maximum index.MaxLimit
	Offset int    `json:"offset,omitempty"`
}

// SearchOut is one page of results, newest first.
type SearchOut struct {
	Messages []MessageOut `json:"messages"`
	More     bool         `json:"more"` // further results exist past this page
}

// DefaultLimit is the page size when none is given.
const DefaultLimit = 50

// MsgSearch finds messages.
var MsgSearch = Op[SearchIn, SearchOut]{
	Name:    "msg.search",
	Summary: "Search messages",
	Perm:    Read,
	Run: func(ctx context.Context, env *Env, in SearchIn) (SearchOut, error) {
		q, err := query.Parse(in.Query)
		if err != nil {
			return SearchOut{}, usageError("%v", err)
		}
		limit := in.Limit
		if limit <= 0 {
			limit = DefaultLimit
		}
		if limit > index.MaxLimit || in.Offset < 0 {
			return SearchOut{}, usageError("limit must be at most %d and offset not negative", index.MaxLimit)
		}
		ix, err := env.Index(ctx)
		if err != nil {
			return SearchOut{}, err
		}
		msgs, err := ix.Search(ctx, q, limit+1, in.Offset)
		if err != nil {
			return SearchOut{}, err
		}
		out := SearchOut{Messages: []MessageOut{}, More: len(msgs) > limit}
		var hashes []string
		for _, m := range msgs[:min(len(msgs), limit)] {
			out.Messages = append(out.Messages, messageOut(m))
			hashes = append(hashes, m.Hash)
		}
		return out, addServerLocations(ctx, env, ix, out.Messages, hashes)
	},
}

// ShowIn identifies a message.
type ShowIn struct {
	ID      string `json:"id"`
	Account string `json:"account,omitempty"` // disambiguates a message stored in several accounts
	Raw     bool   `json:"raw,omitempty"`     // include the raw RFC 5322 message
}

// ShowOut is a message's readable content. HTML is never rendered: an
// HTML-only message is converted to text.
type ShowOut struct {
	Message     MessageOut           `json:"message"`
	Body        string               `json:"body"`
	BodyFrom    string               `json:"body_from"` // "text/plain", "text/html" or "" if none
	Attachments []mailmsg.Attachment `json:"attachments"`
	Raw         string               `json:"raw,omitempty"`
}

// MsgShow reads one message. It changes nothing: reading does not mark a
// message read.
var MsgShow = Op[ShowIn, ShowOut]{
	Name:    "msg.show",
	Summary: "Show a message",
	Perm:    Read,
	Run: func(ctx context.Context, env *Env, in ShowIn) (ShowOut, error) {
		m, err := resolve(ctx, env, in.Account, in.ID)
		if err != nil {
			return ShowOut{}, err
		}
		st, err := env.Store(m.Account)
		if err != nil {
			return ShowOut{}, err
		}
		raw, err := st.Get(m.Hash)
		if errors.Is(err, store.ErrNotFound) {
			return ShowOut{}, fmt.Errorf("message %s is indexed but its file is missing (run 'nou reindex'): %w", m.Hash[:IDLen], ErrNotFound)
		}
		if err != nil {
			return ShowOut{}, err
		}
		content, err := mailmsg.Bodies(raw)
		if err != nil {
			// The error text may quote message content, so it is not logged.
			env.Log.Debug("body parse problem", "id", m.Hash[:IDLen])
		}
		out := ShowOut{Message: messageOut(m), Attachments: content.Attachments}
		ix, err := env.Index(ctx)
		if err != nil {
			return ShowOut{}, err
		}
		msgs := []MessageOut{out.Message}
		if err := addServerLocations(ctx, env, ix, msgs, []string{m.Hash}); err != nil {
			return ShowOut{}, err
		}
		out.Message = msgs[0]
		if out.Attachments == nil {
			out.Attachments = []mailmsg.Attachment{}
		}
		switch {
		case content.Plain != "":
			out.Body, out.BodyFrom = content.Plain, "text/plain"
		case content.HTML != "":
			out.Body, out.BodyFrom = render.HTMLToText(content.HTML), "text/html"
		}
		if in.Raw {
			out.Raw = string(raw)
		}
		return out, nil
	},
}

func resolve(ctx context.Context, env *Env, account, id string) (index.Message, error) {
	ix, err := env.Index(ctx)
	if err != nil {
		return index.Message{}, err
	}
	m, err := ix.Resolve(ctx, account, id)
	if amb, ok := errors.AsType[*index.AmbiguousError](err); ok {
		return m, fmt.Errorf("%w (use a longer id or --account): %w", amb, ErrAmbiguous)
	}
	if errors.Is(err, index.ErrNotFound) {
		return m, fmt.Errorf("%w: %w", err, ErrNotFound)
	}
	if err != nil {
		return m, usageError("%v", err)
	}
	return m, nil
}

// TagIn changes tags on every message matching a query.
type TagIn struct {
	Query   string   `json:"query"`
	Account string   `json:"account,omitempty"` // narrows a bare message id
	Add     []string `json:"add,omitempty"`
	Remove  []string `json:"remove,omitempty"`
	DryRun  bool     `json:"dry_run,omitempty"`
}

// TagOut reports what changed.
type TagOut struct {
	Matched int  `json:"matched"`
	Changed int  `json:"changed"`
	DryRun  bool `json:"dry_run"`
}

// MsgTag adds and removes tags. Removing "unread" marks messages read, and
// the Maildir flags follow, so other mail programs see the change.
var MsgTag = Op[TagIn, TagOut]{
	Name:    "msg.tag",
	Summary: "Add or remove tags on messages matching a query",
	Perm:    Tag,
	Run:     runTag,
}

// MsgArchive is Inbox Zero's main gesture: it removes the inbox tag.
var MsgArchive = Op[ArchiveIn, TagOut]{
	Name:    "msg.archive",
	Summary: "Archive messages (remove the inbox tag)",
	Perm:    Tag,
	Run: func(ctx context.Context, env *Env, in ArchiveIn) (TagOut, error) {
		return runTag(ctx, env, TagIn{Query: in.Query, Account: in.Account, Remove: []string{TagInbox}, DryRun: in.DryRun})
	},
}

// ArchiveIn selects messages to archive.
type ArchiveIn struct {
	Query   string `json:"query"`
	Account string `json:"account,omitempty"` // narrows a bare message id
	DryRun  bool   `json:"dry_run,omitempty"`
}

func runTag(ctx context.Context, env *Env, in TagIn) (out TagOut, err error) {
	out.DryRun = in.DryRun
	if len(in.Add) == 0 && len(in.Remove) == 0 {
		return out, usageError("nothing to do: give tags to add or remove")
	}
	for _, t := range slices.Concat(in.Add, in.Remove) {
		if err := index.ValidateTag(t); err != nil {
			return out, usageError("%v", err)
		}
	}
	ix, err := env.Index(ctx)
	if err != nil {
		return out, err
	}
	q, err := queryOrID(ctx, env, in.Query, in.Account)
	if err != nil {
		return out, err
	}

	// Whatever happens, tags already changed must reach the dump, and the
	// caller must learn that some messages were changed.
	defer func() {
		if in.DryRun || out.Changed == 0 {
			return
		}
		if dumpErr := writeTagsDump(ctx, env); dumpErr != nil {
			err = errors.Join(err, dumpErr)
		}
		if err != nil && !errors.Is(err, ErrPartial) {
			err = fmt.Errorf("%d messages were changed before the failure: %w: %w", out.Changed, err, ErrPartial)
		}
	}()

	var flagErrs []error
	err = ix.Each(ctx, q, func(page []index.Message) error {
		var ids []int64
		var changed []index.Message
		for _, m := range page {
			out.Matched++
			next := applyTagChange(m.Tags, in.Add, in.Remove)
			if slices.Equal(next, m.Tags) {
				continue
			}
			ids = append(ids, m.ID)
			m.Tags = next
			changed = append(changed, m)
		}
		if in.DryRun {
			out.Changed += len(ids)
			return nil
		}
		if len(ids) == 0 {
			return nil
		}
		if err := ix.ModifyTags(ctx, ids, in.Add, in.Remove); err != nil {
			return err
		}
		out.Changed += len(ids)
		if err := applyFlags(env, changed); err != nil {
			flagErrs = append(flagErrs, err)
		}
		return ctx.Err()
	})
	if err != nil {
		return out, err
	}
	if err := errors.Join(flagErrs...); err != nil {
		return out, fmt.Errorf("tags changed, but some file flags could not be updated: %w: %w", err, ErrPartial)
	}
	return out, nil
}

// queryOrID parses a query. A bare message ID is accepted as a convenience
// and must identify exactly one message, like msg show. An empty query is
// refused: it would match every message.
func queryOrID(ctx context.Context, env *Env, s, account string) (query.Query, error) {
	s = strings.TrimSpace(s)
	if len(s) >= index.MinPrefix && len(s) <= 64 && isLowerHex(s) {
		m, err := resolve(ctx, env, account, s)
		if errors.Is(err, ErrNotFound) {
			return query.Query{}, fmt.Errorf("no message with id %s (to search for the word, quote it: '\"%s\"'): %w", s, s, ErrNotFound)
		}
		if err != nil {
			return query.Query{}, err
		}
		s = "hash:" + m.Hash + " account:" + m.Account
	} else if account != "" {
		return query.Query{}, usageError("--account only applies to a message id; use account: in the query")
	}
	q, err := query.Parse(s)
	if err != nil {
		return q, usageError("%v", err)
	}
	if len(q.Groups) == 0 {
		return q, usageError("a query is required (e.g. 'tag:inbox from:bob' or a message id); it may not be empty")
	}
	return q, nil
}

func isLowerHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func applyTagChange(tags, add, remove []string) []string {
	var out []string
	for _, t := range tags {
		if !slices.Contains(remove, t) {
			out = append(out, t)
		}
	}
	return mergeTags(out, add)
}

// addServerLocations fills MessageOut.Server for a page of messages, with
// one index query per account.
func addServerLocations(ctx context.Context, env *Env, ix *index.Index, msgs []MessageOut, hashes []string) error {
	cfg, err := env.Config()
	if err != nil {
		return err
	}
	byAccount := map[string][]int{}
	for i, m := range msgs {
		byAccount[m.Account] = append(byAccount[m.Account], i)
	}
	for account, idx := range byAccount {
		hs := make([]string, len(idx))
		for j, i := range idx {
			hs[j] = hashes[i]
		}
		copies, err := ix.OnServer(ctx, account, hs)
		if err != nil {
			return err
		}
		a, configured := cfg.Account(account)
		for _, i := range idx {
			for _, c := range copies[hashes[i]] {
				loc := ServerLocation{Folder: c.FolderName}
				if configured && a.DeletionEnabled() {
					if p := a.Policy(c.FolderName); p.Action == config.ActionArchive {
						t := c.InternalDate.Add(time.Duration(p.KeepDays) * 24 * time.Hour)
						loc.LeavesAt = &t
					}
				}
				msgs[i].Server = append(msgs[i].Server, loc)
			}
		}
	}
	return nil
}
