// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/index"
	"github.com/pluja/nought/internal/mailmsg"
	"github.com/pluja/nought/internal/query"
	"github.com/pluja/nought/internal/render"
	"github.com/pluja/nought/internal/store"
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

// messageOut is what frontends (and agents) get of a message. Text from
// the mail is cleaned of control, bidi and invisible characters, which could
// hide instructions to an agent or spoof what a person sees.
func messageOut(m index.Message) MessageOut {
	to := make([]string, len(m.To))
	for i, t := range m.To {
		to[i] = render.SanitizeLine(t)
	}
	out := MessageOut{
		ID: m.Hash[:IDLen], Account: m.Account, From: render.SanitizeLine(m.From), To: to, Subject: render.SanitizeLine(m.Subject),
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
		q, err := parseWithoutTrash(in.Query)
		if err != nil {
			return SearchOut{}, usageError("%v", err)
		}
		limit := in.Limit
		if limit == 0 {
			limit = DefaultLimit
		}
		if limit < 0 || limit > index.MaxLimit || in.Offset < 0 {
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
	// Prefer picks the version of a message that has both: "html" or
	// "plain"; empty uses the configured prefer_body (default html).
	Prefer string `json:"prefer,omitempty"`
}

// ShowOut is a message's readable content. HTML is never rendered: an
// HTML-only message is converted to text.
type ShowOut struct {
	Message  MessageOut `json:"message"`
	Body     string     `json:"body"`
	BodyFrom string     `json:"body_from"` // "text/plain", "text/html" or "" if none
	// Alternative is the other version available ("text/plain" or
	// "text/html"), if the message has both.
	Alternative string               `json:"alternative,omitempty"`
	Attachments []mailmsg.Attachment `json:"attachments"`
	Raw         string               `json:"raw,omitempty"`
	// Source is the original body part (HTML or plain text), for frontends
	// that render it themselves. It is not part of the JSON output.
	Source string `json:"-"`
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
		st, err := env.locate(m.Account, m.Hash)
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
		for i, a := range out.Attachments {
			out.Attachments[i].Filename, out.Attachments[i].ContentType = render.SanitizeLine(a.Filename), render.SanitizeLine(a.ContentType)
		}
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
		prefer, err := preferredBody(env, in.Prefer)
		if err != nil {
			return ShowOut{}, err
		}
		var htmlText string
		wantHTML := content.HTML != "" && (prefer == config.BodyHTML || content.Plain == "")
		if wantHTML {
			htmlText = render.HTMLToText(content.HTML)
		}
		// Unless asked for explicitly, HTML that shows nothing (images
		// only, all hidden) or cannot be converted gives way to the plain
		// version, if there is one.
		htmlUsable := in.Prefer == config.BodyHTML || strings.TrimSpace(htmlText) != "" && htmlText != render.Unreadable
		switch {
		case wantHTML && (htmlUsable || content.Plain == ""):
			out.Body, out.BodyFrom, out.Source = htmlText, "text/html", content.HTML
			if content.Plain != "" {
				out.Alternative = "text/plain"
			}
		case content.Plain != "":
			out.Body, out.BodyFrom, out.Source = content.Plain, "text/plain", content.Plain
			if content.HTML != "" {
				out.Alternative = "text/html"
			}
		}
		// Cleaned like the headers; Source and Raw keep the original bytes
		// for those who render or pipe them themselves.
		out.Body = render.Sanitize(out.Body)
		if in.Raw {
			out.Raw = string(raw)
		}
		return out, nil
	},
}

// preferredBody resolves which version to show of a message with both.
func preferredBody(env *Env, prefer string) (string, error) {
	switch prefer {
	case config.BodyHTML, config.BodyPlain:
		return prefer, nil
	case "":
	default:
		return "", usageError("prefer must be %q or %q", config.BodyHTML, config.BodyPlain)
	}
	cfg, err := env.Config()
	if err != nil || cfg.PreferBody == "" {
		return config.BodyHTML, err
	}
	return cfg.PreferBody, nil
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

// MaxBatch is the most messages one tag or delete names (Messages), and so
// the most a frontend selects for one.
const MaxBatch = index.MaxLimit

// MessageRef names one message: its id, as nou shows it, and its account
// (ids are unique within an account; without one, an id must be unique
// across accounts).
type MessageRef struct {
	ID      string `json:"id"`
	Account string `json:"account,omitempty"`
}

// TagIn changes tags on every message matching a query, or on exactly the
// messages listed (a batch; the trash included, as for a bare id).
type TagIn struct {
	Query    string       `json:"query,omitempty"`
	Account  string       `json:"account,omitempty"` // narrows a bare message id
	Messages []MessageRef `json:"messages,omitempty"`
	Add      []string     `json:"add,omitempty"`
	Remove   []string     `json:"remove,omitempty"`
	DryRun   bool         `json:"dry_run,omitempty"`
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
	// Adding "deleted" or a delete-after date deletes mail for good later,
	// here and on the server; taking them away is always allowed.
	PermFor: func(in TagIn) Permission {
		if slices.ContainsFunc(in.Add, IsDeletionTag) {
			return Delete
		}
		return Tag
	},
	Run: runTag,
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
	each, err := selection(ctx, env, in.Query, in.Account, in.Messages)
	if err != nil {
		return out, err
	}

	// Whatever happens, tags already changed must reach the dump, and the
	// caller must learn that some messages were changed.
	dumped := false
	defer func() {
		if in.DryRun || out.Changed == 0 || dumped {
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
	var moves []index.Message // into or out of the transient store
	err = each(func(page []index.Message) error {
		var ids []int64
		var changed []index.Message
		for _, m := range page {
			out.Matched++
			next := applyTagChange(m.Tags, in.Add, in.Remove)
			if slices.Equal(next, m.Tags) {
				continue
			}
			ids = append(ids, m.ID)
			if isTransient(next) != isTransient(m.Tags) {
				moves = append(moves, index.Message{ID: m.ID, Account: m.Account, Hash: m.Hash, Tags: next})
			}
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
	// Files move only once the dumps record the new tags, so an
	// interrupted move is undone by reindex, not the other way round. A dry
	// run moves nothing.
	if len(moves) > 0 && !in.DryRun {
		dumped = true
		if err := writeTagsDump(ctx, env); err != nil {
			return out, fmt.Errorf("%d messages were changed before the failure: %w: %w", out.Changed, err, ErrPartial)
		}
		if err := placeFiles(env, moves); err != nil {
			flagErrs = append(flagErrs, err)
		}
	}
	if err := errors.Join(flagErrs...); err != nil {
		return out, fmt.Errorf("tags changed, but some file flags could not be updated: %w: %w", err, ErrPartial)
	}
	return out, nil
}

// queryOrID parses a query. A bare message ID is accepted as a convenience
// and must identify exactly one message, like msg show. An empty query is
// refused: it would match every message.
// queryOrID parses a query, or resolves a bare message id (any case) to
// that exact message. A query leaves out the trash unless it asks for it
// (tag:deleted), as search does; an id means that message, trashed or not.
func queryOrID(ctx context.Context, env *Env, s, account string) (query.Query, error) {
	return parseQueryOrID(ctx, env, s, account, true)
}

// queryOrIDWithTrash is queryOrID for actions on the trash (restore).
func queryOrIDWithTrash(ctx context.Context, env *Env, s, account string) (query.Query, error) {
	return parseQueryOrID(ctx, env, s, account, false)
}

func parseQueryOrID(ctx context.Context, env *Env, s, account string, hideTrash bool) (query.Query, error) {
	s = strings.TrimSpace(s)
	byID := len(s) >= index.MinPrefix && len(s) <= 64 && isHex(s)
	if byID {
		m, err := resolve(ctx, env, account, strings.ToLower(s))
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
	if hideTrash && !byID {
		q = withoutTrash(q)
	}
	return q, nil
}

func isHex(s string) bool {
	for _, c := range strings.ToLower(s) {
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
					loc.LeavesAt = policyLeavesAt(a.Policy(c.FolderName), c.InternalDate, slices.Contains(msgs[i].Tags, TagInbox))
				}
				if t, ok := leavesServerAt(msgs[i].Tags, env.Now()); ok {
					loc.LeavesAt = earliest(loc.LeavesAt, t)
				}
				msgs[i].Server = append(msgs[i].Server, loc)
			}
		}
	}
	return nil
}

// policyLeavesAt is when sync deletes a copy under its folder's policy:
// once the message is handled (out of the inbox), after keep_days; in any
// case after max_days, if set. Nil: it stays.
func policyLeavesAt(p config.Policy, received time.Time, inInbox bool) *time.Time {
	if p.Action != config.ActionArchive {
		return nil
	}
	var at *time.Time
	if !inInbox {
		at = earliest(at, received.Add(days(p.KeepDays)))
	}
	if p.MaxDays > 0 {
		at = earliest(at, received.Add(days(p.MaxDays)))
	}
	return at
}

// earliest returns the earlier of at (nil: none) and t.
func earliest(at *time.Time, t time.Time) *time.Time {
	if at == nil || t.Before(*at) {
		return &t
	}
	return at
}

// CountIn is a query to count.
type CountIn struct {
	Query string `json:"query"`
}

// CountOut is how many messages match.
type CountOut struct {
	Count int `json:"count"`
}

// MsgCount counts messages matching a query, without loading them.
var MsgCount = Op[CountIn, CountOut]{
	Name:    "msg.count",
	Summary: "Count messages matching a query",
	Perm:    Read,
	Run: func(ctx context.Context, env *Env, in CountIn) (CountOut, error) {
		q, err := parseWithoutTrash(in.Query)
		if err != nil {
			return CountOut{}, usageError("%v", err)
		}
		ix, err := env.Index(ctx)
		if err != nil {
			return CountOut{}, err
		}
		n, err := ix.Count(ctx, q)
		return CountOut{Count: n}, err
	},
}

// parseWithoutTrash parses a search, hiding deleted mail unless the query
// has a tag:deleted term.
func parseWithoutTrash(s string) (query.Query, error) {
	q, err := query.Parse(s)
	if err != nil {
		return q, err
	}
	return withoutTrash(q), nil
}

// withoutTrash leaves the trash out of q, unless q asks for it or names
// messages (hash:, id:), which means them, trashed or not.
func withoutTrash(q query.Query) query.Query {
	for _, g := range q.Groups {
		for _, t := range g {
			if t.Field == query.Tag && t.Value == TagDeleted || t.Field == query.Hash || t.Field == query.ID {
				return q
			}
		}
	}
	q.Groups = append(q.Groups, []query.Term{{Field: query.Tag, Value: TagDeleted, Not: true}})
	return q
}

// namedPage is how many named messages an action handles at a time.
const namedPage = 1000

// selection walks, a page at a time, the messages an action applies to: a
// query's matches, or exactly the messages named. Named ones are all
// resolved first, so an unknown id changes nothing.
func selection(ctx context.Context, env *Env, q, account string, refs []MessageRef) (func(func([]index.Message) error) error, error) {
	ix, err := env.Index(ctx)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		parsed, err := queryOrID(ctx, env, q, account)
		if err != nil {
			return nil, err
		}
		return func(fn func([]index.Message) error) error { return ix.Each(ctx, parsed, fn) }, nil
	}
	switch {
	case q != "" || account != "":
		return nil, usageError("give a query or a list of messages, not both")
	case len(refs) > MaxBatch:
		return nil, usageError("at most %d messages at once", MaxBatch)
	}
	msgs := make([]index.Message, 0, len(refs))
	seen := map[int64]bool{}
	for _, r := range refs {
		m, err := resolve(ctx, env, r.Account, r.ID)
		if err != nil {
			return nil, err
		}
		if !seen[m.ID] {
			seen[m.ID] = true
			msgs = append(msgs, m)
		}
	}
	return func(fn func([]index.Message) error) error {
		for page := range slices.Chunk(msgs, namedPage) {
			if err := fn(page); err != nil {
				return err
			}
		}
		return nil
	}, nil
}
