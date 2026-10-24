// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"

	"github.com/pluja/nought/internal/atomicfile"
	"github.com/pluja/nought/internal/index"
	"github.com/pluja/nought/internal/mailmsg"
	"github.com/pluja/nought/internal/render"
	"github.com/pluja/nought/internal/store"
)

// Well-known tags.
const (
	TagInbox   = "inbox"
	TagUnread  = "unread"
	TagFlagged = "flagged"
	TagReplied = "replied"
)

// archive stores raw in the account's Maildir (verified), indexes it and,
// if it is new, applies the rules. With keep, it is the only way mail
// enters the archive: sync and sending use archive, import uses keep.
func archive(ctx context.Context, env *Env, account string, raw []byte, flags string, tags []string) (index.Message, bool, error) {
	m, created, err := keep(ctx, env, account, raw, flags, tags)
	if err == nil && created {
		ix, err := env.Index(ctx)
		if err != nil {
			return m, created, err
		}
		return m, created, applyRules(ctx, env, ix, &m)
	}
	return m, created, err
}

// keep stores and indexes a message without applying the rules: imported
// mail is old mail, which rules (an expiry above all) must not touch.
func keep(ctx context.Context, env *Env, account string, raw []byte, flags string, tags []string) (index.Message, bool, error) {
	// Into the store that already holds it, if any: a trashed message
	// fetched again must not get a second file in the archive (backed up,
	// and missed by purge).
	st, err := env.locate(account, store.Hash(raw))
	if err != nil {
		return index.Message{}, false, err
	}
	entry, _, err := st.Put(raw, flags)
	if err != nil {
		return index.Message{}, false, fmt.Errorf("store message: %w", err)
	}
	ix, err := env.Index(ctx)
	if err != nil {
		return index.Message{}, false, err
	}
	m, created, err := indexMessage(ctx, env, ix, account, raw, entry.Flags, tags)
	if err != nil || !created {
		return m, created, err
	}
	// New to the index, yet already in a store: one its tags may not call
	// for (a file left in the trash store by a crash or a lost index).
	transient, err := env.Transient(account)
	if err != nil {
		return m, created, err
	}
	if (st == transient) != isTransient(m.Tags) {
		err = placeFiles(env, []index.Message{m})
	}
	return m, created, err
}

// indexMessage adds a stored message to the index. Parsing is best effort:
// a message that cannot be parsed is still indexed by its hash.
func indexMessage(ctx context.Context, env *Env, ix *index.Index, account string, raw []byte, flags string, tags []string) (index.Message, bool, error) {
	hash := store.Hash(raw)
	// Parse errors may quote message content, so they are not logged.
	h, err := mailmsg.Parse(raw)
	if err != nil {
		env.Log.Debug("header parse problem", "account", account, "id", hash[:IDLen])
	}
	content, err := mailmsg.Bodies(raw)
	if err != nil {
		env.Log.Debug("body parse problem", "account", account, "id", hash[:IDLen])
	}
	body := content.Plain
	if body == "" && content.HTML != "" {
		body = render.HTMLToText(content.HTML)
	}
	m := index.Message{
		Account:   account,
		Hash:      hash,
		MessageID: h.MessageID,
		Date:      h.Date,
		From:      h.From,
		To:        append(h.To, h.Cc...), // the index keeps To and Cc together
		Subject:   h.Subject,
		Size:      int64(len(raw)),
		ThreadKey: mailmsg.ThreadKey(h, hash),
		Tags:      mergeTags(tags, flagTags(flags)),
	}
	id, created, err := ix.AddMessage(ctx, m, body, env.Now())
	if err != nil {
		return index.Message{}, false, fmt.Errorf("index message: %w", err)
	}
	m.ID = id
	return m, created, nil
}

// flagTags derives tags from Maildir flags.
func flagTags(flags string) []string {
	var tags []string
	if !strings.Contains(flags, "S") {
		tags = append(tags, TagUnread)
	}
	if strings.Contains(flags, "F") {
		tags = append(tags, TagFlagged)
	}
	if strings.Contains(flags, "R") {
		tags = append(tags, TagReplied)
	}
	return tags
}

// tagFlags maps tags back to Maildir flags, keeping flags nought does not
// manage (D, P, T) as they are.
func tagFlags(current string, tags []string) string {
	var b strings.Builder
	for _, f := range current {
		if !strings.ContainsRune("SFR", f) {
			b.WriteRune(f)
		}
	}
	if !slices.Contains(tags, TagUnread) {
		b.WriteRune('S')
	}
	if slices.Contains(tags, TagFlagged) {
		b.WriteRune('F')
	}
	if slices.Contains(tags, TagReplied) {
		b.WriteRune('R')
	}
	return b.String()
}

func mergeTags(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		for _, t := range l {
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	slices.Sort(out)
	return out
}

// folderTag turns a server or import folder name into a tag:
// "Sent Items" becomes "folder:sent-items".
func folderTag(folder string) string {
	var b strings.Builder
	b.WriteString("folder:")
	dash := false
	for _, r := range strings.ToLower(folder) {
		switch {
		case unicode.IsSpace(r) || unicode.IsControl(r):
			dash = b.Len() > len("folder:")
		default:
			if dash {
				b.WriteByte('-')
				dash = false
			}
			b.WriteRune(r)
		}
	}
	tag := b.String()
	if len(tag) > index.MaxTagLen {
		tag = strings.ToValidUTF8(tag[:index.MaxTagLen], "")
	}
	if tag == "folder:" {
		tag = "folder:unnamed"
	}
	return tag
}

// writeTagsDump rewrites the tag dumps atomically, so the index is never the
// only copy of the user's tags. Deleted and expiring mail goes to its own
// dump in the transient folder, which is kept out of backups.
func writeTagsDump(ctx context.Context, env *Env) error {
	// Tags already changed must reach the dump, their lasting copy, even
	// when the operation that changed them is being cancelled (Ctrl-C).
	ctx = context.WithoutCancel(ctx)
	env.dumpMu.Lock()
	defer env.dumpMu.Unlock()
	ix, err := env.Index(ctx)
	if err != nil {
		return err
	}
	for _, d := range []struct {
		path string
		keep func([]string) bool
	}{
		{env.Paths.TagsDump, func(tags []string) bool { return !isTransient(tags) }},
		{env.Paths.TransientDump, isTransient},
	} {
		err := atomicfile.Write(d.path, 0o600, func(w io.Writer) error { return ix.Dump(ctx, w, d.keep) })
		if err != nil {
			return fmt.Errorf("write tag dump: %w", err)
		}
	}
	return nil
}
