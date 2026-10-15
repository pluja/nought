// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"nought/internal/ops"
)

// Messages produced by background work.
type (
	searchDoneMsg struct {
		query string
		out   ops.SearchOut
		err   error
	}
	showDoneMsg struct {
		id  string
		out ops.ShowOut
		err error
	}
	tagDoneMsg struct {
		out    ops.TagOut
		undo   *undoEntry
		notice string
		err    error
	}
	statusDoneMsg struct {
		out []ops.AccountStatusOut
	}
	sidebarDoneMsg struct {
		items []sideItem
	}
	syncProgressMsg struct{ line string }
	syncDoneMsg     struct {
		out ops.SyncOut
		err error
	}
	reindexDoneMsg struct {
		out ops.ReindexOut
		err error
	}
)

// pageSize is how many messages a view loads.
const pageSize = 500

func (m Model) search(query string) tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, perm, ops.MsgSearch, ops.SearchIn{Query: query, Limit: pageSize})
		return searchDoneMsg{query: query, out: out, err: err}
	}
}

func (m Model) show(msg ops.MessageOut) tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, perm, ops.MsgShow, ops.ShowIn{ID: msg.ID, Account: msg.Account})
		return showDoneMsg{id: msg.ID, out: out, err: err}
	}
}

// messageQuery addresses exactly one message.
func messageQuery(msg ops.MessageOut) string {
	return "hash:" + msg.ID + " account:" + msg.Account
}

// tag changes tags on one message and prepares the matching undo.
func (m Model) tag(msg ops.MessageOut, add, remove []string, what string) tea.Cmd {
	// Undo only what actually changes.
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
	q := messageQuery(msg)
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, perm, ops.MsgTag, ops.TagIn{Query: q, Add: add, Remove: remove})
		var undo *undoEntry
		if err == nil && out.Changed > 0 && what != "" {
			undo = &undoEntry{query: q, add: undoAdd, remove: undoRemove, what: what}
		}
		return tagDoneMsg{out: out, undo: undo, notice: what, err: err}
	}
}

func (m Model) revert(u undoEntry) tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, perm, ops.MsgTag, ops.TagIn{Query: u.query, Add: u.add, Remove: u.remove})
		return tagDoneMsg{out: out, notice: "undid: " + u.what, err: err}
	}
}

func (m Model) loadStatus() tea.Cmd {
	return func() tea.Msg {
		out, _ := ops.Call(m.ctx, m.env, perm, ops.AccountStatus, struct{}{})
		return statusDoneMsg{out: out}
	}
}

// loadSidebar builds the sidebar: standard views, accounts, saved searches
// and tags, with counts where they are cheap.
func (m Model) loadSidebar() tea.Cmd {
	prefs := m.prefs
	return func() tea.Msg {
		count := func(q string) int {
			out, err := ops.Call(m.ctx, m.env, perm, ops.MsgSearch, ops.SearchIn{Query: q, Limit: pageSize})
			if err != nil {
				return -1
			}
			return len(out.Messages)
		}
		items := []sideItem{
			{label: "Inbox", query: queryInbox, count: count(queryInbox)},
			{label: "Unread", query: queryUnread, count: count(queryUnread)},
			{label: "Flagged", query: queryFlagged, count: count(queryFlagged)},
			{label: "All mail", query: queryAll, count: -1},
		}
		if accounts, err := ops.Call(m.ctx, m.env, perm, ops.AccountList, struct{}{}); err == nil && len(accounts) > 1 {
			for _, a := range accounts {
				items = append(items, sideItem{label: "@" + a.Name, query: "account:" + a.Name, count: -1})
			}
		}
		for _, s := range prefs.Searches {
			items = append(items, sideItem{label: "★ " + s.Name, query: s.Query, count: -1})
		}
		if tags, err := ops.Call(m.ctx, m.env, perm, ops.TagsList, struct{}{}); err == nil {
			for _, t := range tags {
				switch t.Tag {
				case ops.TagInbox, ops.TagUnread, ops.TagFlagged:
					continue
				}
				items = append(items, sideItem{label: "#" + t.Tag, query: "tag:" + t.Tag, count: t.Count})
			}
		}
		return sidebarDoneMsg{items: items}
	}
}

func (m Model) sync() tea.Cmd {
	send := m.send
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, perm, ops.SyncRun, ops.SyncIn{
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

func (m Model) reindex() tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, perm, ops.IndexRebuild, struct{}{})
		return reindexDoneMsg{out: out, err: err}
	}
}

// syncSummary condenses a sync result into one line.
func syncSummary(out ops.SyncOut, err error) string {
	fetched, expunged, problems := 0, 0, 0
	for _, a := range out.Accounts {
		if a.Error != "" {
			problems++
		}
		for _, f := range a.Folders {
			fetched += f.Fetched
			expunged += f.Expunged
			if f.Error != "" {
				problems++
			}
		}
	}
	s := fmt.Sprintf("synced: archived %d, deleted %d from servers", fetched, expunged)
	if problems > 0 || err != nil {
		s += fmt.Sprintf("; %d problems (run 'nou sync' for details)", max(problems, 1))
	}
	return s
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
