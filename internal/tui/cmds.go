// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pluja/nought/internal/ops"
)

// Messages produced by background work.
type (
	searchDoneMsg struct {
		gen int
		out ops.SearchOut
		err error
	}
	showDoneMsg struct {
		gen    int
		id     string
		prefer string // the version asked for with v; "" for ordinary shows
		out    ops.ShowOut
		err    error
	}
	previewMsg struct{ seq int }
	tagDoneMsg struct {
		req tagReq
		out ops.TagOut
		err error
	}
	undoDoneMsg struct {
		entry undoEntry
		err   error
	}
	statusDoneMsg   struct{ out []ops.AccountStatusOut }
	sidebarDoneMsg  struct{ items []sideItem }
	syncProgressMsg struct{ line string }
	syncDoneMsg     struct {
		out ops.SyncOut
		err error
	}
	deleteDoneMsg struct {
		out ops.DeleteOut
		err error
	}
	trashEmptyDoneMsg struct {
		out ops.TrashEmptyOut
		err error
	}
	reindexDoneMsg struct {
		out ops.ReindexOut
		err error
	}
)

// pageSize is how many messages a view loads.
const pageSize = 500

// previewDelay debounces previews while the cursor moves.
const previewDelay = 80 * time.Millisecond

func (m Model) search(query string, gen int) tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, permTag, ops.MsgSearch, ops.SearchIn{Query: query, Limit: pageSize})
		return searchDoneMsg{gen: gen, out: out, err: err}
	}
}

func (m Model) show(msg ops.MessageOut, gen int) tea.Cmd { return m.showVersion(msg, gen, "") }

// showVersion shows a message; prefer, when set, asks for one version and
// becomes the session's choice once it is shown.
func (m Model) showVersion(msg ops.MessageOut, gen int, prefer string) tea.Cmd {
	in := ops.ShowIn{ID: msg.ID, Account: msg.Account, Prefer: cmp.Or(prefer, m.bodyPrefer)}
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, permTag, ops.MsgShow, in)
		return showDoneMsg{gen: gen, id: msg.ID, prefer: prefer, out: out, err: err}
	}
}

// messageQuery addresses exactly one message.
func messageQuery(msg ops.MessageOut) string {
	return "hash:" + msg.ID + " account:" + msg.Account
}

// newTagReq prepares a tag change and the exact undo for it.
func newTagReq(msg ops.MessageOut, add, remove []string, what string) tagReq {
	req := tagReq{msg: msg, add: add, remove: remove, what: what}
	if what == "" {
		return req
	}
	var undoAdd, undoRemove []string
	for _, t := range remove {
		if slices.Contains(msg.Tags, t) {
			undoAdd = append(undoAdd, t)
		}
	}
	for _, t := range add {
		if !slices.Contains(msg.Tags, t) {
			undoRemove = append(undoRemove, t)
		}
	}
	req.undo = &undoEntry{query: messageQuery(msg), add: undoAdd, remove: undoRemove,
		what: what + ": " + msg.Subject}
	return req
}

func (m Model) runTag(req tagReq) tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, tagPerm(req.add), ops.MsgTag,
			ops.TagIn{Query: messageQuery(req.msg), Add: req.add, Remove: req.remove})
		return tagDoneMsg{req: req, out: out, err: err}
	}
}

// tagPerm is the level a tag change needs: moving mail to the trash (or
// undoing a restore) is the user's explicit deletion, so it uses the
// highest; everything else stays at the tag level.
func tagPerm(add []string) ops.Permission {
	if slices.ContainsFunc(add, ops.IsDeletionTag) {
		return permDelete
	}
	return permTag
}

func (m Model) revert(u undoEntry) tea.Cmd {
	return func() tea.Msg {
		_, err := ops.Call(m.ctx, m.env, tagPerm(u.add), ops.MsgTag, ops.TagIn{Query: u.query, Add: u.add, Remove: u.remove})
		return undoDoneMsg{entry: u, err: err}
	}
}

func (m Model) loadStatus() tea.Cmd {
	return func() tea.Msg {
		out, _ := ops.Call(m.ctx, m.env, permTag, ops.AccountStatus, struct{}{})
		return statusDoneMsg{out: out}
	}
}

// loadSidebar builds the sidebar: standard views, accounts, saved searches
// and tags, with exact counts.
func (m Model) loadSidebar() tea.Cmd {
	prefs := m.prefs
	return func() tea.Msg {
		count := func(q string) int {
			out, err := ops.Call(m.ctx, m.env, permTag, ops.MsgCount, ops.CountIn{Query: q})
			if err != nil {
				return -1
			}
			return out.Count
		}
		items := []sideItem{
			{label: "Inbox", query: queryInbox, count: count(queryInbox)},
			{label: "Unread", query: queryUnread, count: count(queryUnread)},
			{label: "Flagged", query: queryFlagged, count: count(queryFlagged)},
			{label: "Archive", query: queryArchive, count: -1},
			{label: "All mail", query: queryAll, count: -1},
		}
		if n := count(queryTrash); n > 0 { // shown only when there is something in it
			items = append(items, sideItem{label: "Trash", query: queryTrash, count: n})
		}
		if accounts, err := ops.Call(m.ctx, m.env, permTag, ops.AccountList, struct{}{}); err == nil && len(accounts) > 1 {
			for _, a := range accounts {
				items = append(items, sideItem{label: "@" + a.Name, query: "account:" + a.Name, count: -1})
			}
		}
		for _, s := range prefs.Searches {
			items = append(items, sideItem{label: "★ " + s.Name, query: s.Query, count: -1})
		}
		if tags, err := ops.Call(m.ctx, m.env, permTag, ops.TagsList, struct{}{}); err == nil {
			// Folder tags say where mail came from; they get their own
			// section. INBOX is left out: the Inbox view is the inbox.
			var folders []sideItem
			for _, t := range tags {
				folder, isFolder := strings.CutPrefix(t.Tag, "folder:")
				switch {
				case t.Tag == ops.TagInbox || t.Tag == ops.TagUnread || t.Tag == ops.TagFlagged || hiddenTag(t.Tag):
				case isFolder && folder != "" && folder != "inbox":
					folders = append(folders, sideItem{label: folder, query: "tag:" + t.Tag, count: t.Count})
				case !isFolder:
					items = append(items, sideItem{label: "#" + t.Tag, query: "tag:" + t.Tag, count: t.Count})
				}
			}
			if len(folders) > 0 {
				folders[0].heading = "Folders"
			}
			items = append(items, folders...)
		}
		return sidebarDoneMsg{items: items}
	}
}

// sync runs in its own cancellable context, so quitting can stop it at a
// safe point (sync finishes bookkeeping for anything already deleted).
func (m Model) sync(ctx context.Context) tea.Cmd {
	send, accept := m.send, m.syncAccept
	return func() tea.Msg {
		out, err := ops.Call(ctx, m.env, permDelete, ops.SyncRun, ops.SyncIn{
			AcceptRemovals: accept,
			Progress: func(account string, f ops.FolderSyncOut) {
				if f.Action == "ignore" {
					return
				}
				send(syncProgressMsg{line: fmt.Sprintf("%s/%s: archived %d, deleted %d", account, f.Folder, f.Fetched, f.Expunged)})
			},
		})
		return syncDoneMsg{out: out, err: err}
	}
}

// deleteNow deletes one message for good (its server copies at the next
// sync).
func (m Model) deleteNow(msg ops.MessageOut) tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, permDelete, ops.MsgDelete, ops.DeleteIn{Query: messageQuery(msg), Now: true})
		return deleteDoneMsg{out: out, err: err}
	}
}

func (m Model) emptyTrash() tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, permDelete, ops.TrashEmpty, struct{}{})
		return trashEmptyDoneMsg{out: out, err: err}
	}
}

func (m Model) reindex() tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, permTag, ops.IndexRebuild, struct{}{})
		return reindexDoneMsg{out: out, err: err}
	}
}

func previewAfter(seq int, wait time.Duration) tea.Cmd {
	if wait <= 0 {
		return func() tea.Msg { return previewMsg{seq: seq} }
	}
	return tea.Tick(wait, func(time.Time) tea.Msg { return previewMsg{seq: seq} })
}

// parseTagInput reads "+a -b c" (a bare word adds).
func parseTagInput(s string) (add, remove []string) {
	for _, f := range strings.Fields(s) {
		switch {
		case strings.HasPrefix(f, "-") && len(f) > 1:
			remove = append(remove, strings.ToLower(f[1:]))
		case strings.HasPrefix(f, "+") && len(f) > 1:
			add = append(add, strings.ToLower(f[1:]))
		case !strings.HasPrefix(f, "+") && !strings.HasPrefix(f, "-"):
			add = append(add, strings.ToLower(f))
		}
	}
	return add, remove
}

// trashReq moves a message to the trash, or takes it out (back to the
// inbox). Both are undoable with z.
func (m Model) trashReq(msg ops.MessageOut) (tagReq, error) {
	var expiry []string
	for _, t := range msg.Tags {
		if ops.IsExpiryTag(t) {
			expiry = append(expiry, t)
		}
	}
	if slices.Contains(msg.Tags, ops.TagDeleted) {
		return newTagReq(msg, []string{ops.TagInbox}, append([]string{ops.TagDeleted}, expiry...), "restored"), nil
	}
	add, remove, err := ops.TrashTags(m.env, false)
	if err != nil {
		return tagReq{}, err
	}
	// An earlier expiry (from a rule) still applies, as with nou msg delete.
	return newTagReq(msg, add, remove, "moved to the trash"), nil
}
