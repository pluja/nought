// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// savedMsg reports attachments written to disk.
type savedMsg struct {
	dir   string
	files []ops.SavedFile
	err   error
}

func (m Model) saveAttachments(msg ops.MessageOut, dir string) tea.Cmd {
	return func() tea.Msg {
		files, err := ops.Call(m.ctx, m.env, permTag, ops.MsgSave, ops.SaveIn{ID: msg.ID, Account: msg.Account, Dir: dir})
		return savedMsg{dir: dir, files: files, err: err}
	}
}

// defaultSaveDir is where attachments go unless the user says otherwise:
// Downloads, if there is one, else the home directory.
func defaultSaveDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	if info, err := os.Stat(filepath.Join(home, "Downloads")); err == nil && info.IsDir() {
		return "~/Downloads"
	}
	return "~/"
}

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

// refs names messages for the ops that take a list.
func refs(msgs []ops.MessageOut) []ops.MessageRef {
	out := make([]ops.MessageRef, len(msgs))
	for i, msg := range msgs {
		out[i] = ops.MessageRef{ID: msg.ID, Account: msg.Account}
	}
	return out
}

// newTagReq prepares a tag change on msgs and the exact undo for it: the
// messages are grouped by what the change does to them, so undoing gives
// each the tags it had.
func newTagReq(msgs []ops.MessageOut, add, remove []string, what string) tagReq {
	req := tagReq{msgs: msgs, add: add, remove: remove, what: what}
	if what == "" {
		return req
	}
	u := &undoEntry{what: what + ": " + about(msgs)}
	groups := map[string]int{}
	for _, msg := range msgs {
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
		if len(undoAdd)+len(undoRemove) == 0 {
			continue
		}
		key := strings.Join(undoAdd, ",") + "|" + strings.Join(undoRemove, ",")
		i, ok := groups[key]
		if !ok {
			i = len(u.steps)
			groups[key] = i
			u.steps = append(u.steps, undoStep{add: undoAdd, remove: undoRemove})
		}
		u.steps[i].msgs = append(u.steps[i].msgs, ops.MessageRef{ID: msg.ID, Account: msg.Account})
	}
	req.undo = u
	return req
}

// about names what an action was on: the subject, or how many.
func about(msgs []ops.MessageOut) string {
	if len(msgs) == 1 {
		return msgs[0].Subject
	}
	return strconv.Itoa(len(msgs)) + " messages"
}

func (m Model) runTag(req tagReq) tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, tagPerm(req.add), ops.MsgTag,
			ops.TagIn{Messages: refs(req.msgs), Add: req.add, Remove: req.remove})
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

// revert undoes a change, step by step. A failed undo is kept to try
// again: each step sets tags, so running one twice does no harm.
func (m Model) revert(u undoEntry) tea.Cmd {
	return func() tea.Msg {
		for _, s := range u.steps {
			_, err := ops.Call(m.ctx, m.env, tagPerm(s.add), ops.MsgTag,
				ops.TagIn{Messages: s.msgs, Add: s.add, Remove: s.remove})
			if err != nil {
				return undoDoneMsg{entry: u, err: err}
			}
		}
		return undoDoneMsg{entry: u}
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
		if drafts, err := ops.Call(m.ctx, m.env, permTag, ops.DraftList, struct{}{}); err == nil && len(drafts) > 0 {
			items = append(items, sideItem{label: "Drafts", query: queryDrafts, count: len(drafts)})
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
				case t.Tag == ops.TagInbox || t.Tag == ops.TagUnread || t.Tag == ops.TagFlagged || t.Tag == ops.TagReplied ||
					hiddenTag(t.Tag):
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

// deleteNow deletes messages for good (their server copies at the next
// sync).
func (m Model) deleteNow(msgs []ops.MessageOut) tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, permDelete, ops.MsgDelete, ops.DeleteIn{Messages: refs(msgs), Now: true})
		return deleteDoneMsg{out: out, err: err}
	}
}

func (m Model) emptyTrash() tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, permDelete, ops.TrashEmpty, ops.TrashEmptyIn{})
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
	for f := range strings.FieldsSeq(s) {
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

// trashReq moves messages to the trash, or, when all of them are in it,
// takes them out (back to the inbox). Both are undoable with z.
func (m Model) trashReq(msgs []ops.MessageOut) (tagReq, error) {
	isTrashed := hasTag(ops.TagDeleted)
	if all(msgs, isTrashed) {
		remove := []string{ops.TagDeleted}
		for _, msg := range msgs {
			for _, t := range msg.Tags {
				if ops.IsExpiryTag(t) && !slices.Contains(remove, t) {
					remove = append(remove, t)
				}
			}
		}
		return newTagReq(msgs, []string{ops.TagInbox}, remove, "restored"), nil
	}
	add, remove, err := ops.TrashTags(m.env, false)
	if err != nil {
		return tagReq{}, err
	}
	// An earlier expiry (from a rule) still applies, as with nou msg delete.
	return newTagReq(slices.DeleteFunc(slices.Clone(msgs), isTrashed), add, remove, "moved to the trash"), nil
}
