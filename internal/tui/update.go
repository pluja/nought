// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/pluja/nought/internal/ops"
)

// maxUndo bounds the undo history.
const maxUndo = 50

// Update handles one event, then fits the layout to the new state.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	next.relayout()
	return next, cmd
}

func (m Model) update(msg tea.Msg) (Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case searchDoneMsg:
		return m.searchDone(msg)

	case showDoneMsg:
		sel, ok := m.selected()
		if msg.gen != m.showGen || !ok || sel.ID != msg.id {
			return m, nil // superseded
		}
		if msg.err != nil {
			m.notice, m.noticeErr = msg.err.Error(), true
			return m, nil
		}
		// The list holds the newest tags (changes apply there at once).
		msg.out.Message.Tags = sel.Tags
		m.open = &msg.out
		if msg.prefer != "" {
			m.bodyPrefer = msg.prefer
			m.notice = "plain-text version (v switches back)"
			if msg.prefer == "html" {
				m.notice = "HTML version, as text (v switches back)"
			}
		}
		return m, nil

	case previewMsg:
		if msg.seq != m.previewSeq {
			return m, nil
		}
		sel, ok := m.selected()
		if !ok || (m.open != nil && m.open.Message.ID == sel.ID) {
			return m, nil
		}
		m.showGen++
		return m, m.show(sel, m.showGen)

	case tagDoneMsg:
		m.tagBusy = false
		switch {
		case msg.err != nil:
			m.notice, m.noticeErr = msg.err.Error(), true
		case msg.req.what != "":
			m.notice = msg.req.what
			if msg.req.note != "" {
				m.notice += "; " + msg.req.note
				if m.deletionNoted == nil {
					m.deletionNoted = map[string]bool{}
				}
				m.deletionNoted[msg.req.msg.Account] = true
			}
			if msg.req.undo != nil && msg.out.Changed > 0 {
				m.undo = append(m.undo, *msg.req.undo)
				if len(m.undo) > maxUndo {
					m.undo = m.undo[1:]
				}
			}
		}
		if len(m.tagQueue) > 0 {
			return m.startNextTag()
		}
		return m.refresh()

	case undoDoneMsg:
		if msg.err != nil {
			m.undo = append(m.undo, msg.entry) // keep it for another try
			m.notice, m.noticeErr = "undo failed: "+msg.err.Error(), true
			return m, nil
		}
		m.notice = "undid " + msg.entry.what
		return m.refresh()

	case statusDoneMsg:
		m.status = msg.out
		return m, nil

	case sidebarDoneMsg:
		m.side = msg.items
		m.sideIdx = min(m.sideIdx, max(len(m.side)-1, 0))
		return m, nil

	case syncProgressMsg:
		m.syncLines = append(m.syncLines, msg.line)
		if len(m.syncLines) > 3 {
			m.syncLines = m.syncLines[len(m.syncLines)-3:]
		}
		return m, nil

	case syncDoneMsg:
		return m.syncDone(msg)

	case autoSyncMsg:
		return m.autoSync(msg)

	case trashEmptyDoneMsg:
		switch {
		case msg.err != nil:
			m.notice, m.noticeErr = "empty trash: "+msg.err.Error(), true
		case msg.out.Pending > 0:
			m.notice = fmt.Sprintf("deleted %d for good; %d go once their server copies are removed at the next sync",
				msg.out.Purged, msg.out.Pending)
		default:
			m.notice = fmt.Sprintf("deleted %d for good", msg.out.Purged)
		}
		return m.refresh()

	case deleteDoneMsg:
		switch {
		case msg.err != nil:
			m.notice, m.noticeErr = "delete: "+msg.err.Error(), true
		case msg.out.Pending > 0:
			m.notice = "deleted; the server copy goes at the next sync, then the local one"
		default:
			m.notice = "deleted for good"
		}
		return m.refresh()

	case reindexDoneMsg:
		if msg.err != nil {
			m.notice, m.noticeErr = "reindex: "+msg.err.Error(), true
		} else {
			m.notice = "reindexed " + strconv.Itoa(msg.out.Messages) + " messages"
		}
		return m.refresh()

	case tea.KeyPressMsg:
		return m.key(msg)
	}
	if m.inputKind != inputNone {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

// searchDone applies new results, keeping the cursor on the same message
// (not the same row) so actions never land on a different message.
func (m Model) searchDone(msg searchDoneMsg) (Model, tea.Cmd) {
	if msg.gen != m.searchGen || m.tagBusy || len(m.tagQueue) > 0 {
		return m, nil // superseded, or local changes still being saved
	}
	if msg.err != nil {
		m.notice, m.noticeErr = "search: "+msg.err.Error(), true
		m.msgs, m.more, m.open = nil, false, nil
		m.focus = focusList
		return m, nil
	}
	prev, hadSel := m.selected()
	m.msgs, m.more = msg.out.Messages, msg.out.More
	if hadSel {
		if i := slices.IndexFunc(m.msgs, func(x ops.MessageOut) bool { return x.ID == prev.ID }); i >= 0 {
			m.cursor = i
		}
	}
	m.cursor = min(max(m.cursor, 0), max(len(m.msgs)-1, 0))
	if len(m.msgs) == 0 {
		m.open = nil
		if m.focus == focusReader {
			m.focus = focusList
		}
		return m, nil
	}
	sel, _ := m.selected()
	if m.open != nil && m.open.Message.ID != sel.ID {
		if m.focus == focusReader {
			// The open message left this view (e.g. archived while
			// reading): move on to the next one, as mail readers do.
			return m.openSelected()
		}
		m.open = nil
	}
	if m.open != nil {
		open := *m.open
		open.Message.Tags, open.Message.Server = sel.Tags, sel.Server
		m.open = &open
	}
	return m.schedulePreview()
}

// refresh reloads the current view, the sidebar and the status bar.
func (m Model) refresh() (Model, tea.Cmd) {
	m.searchGen++
	return m, tea.Batch(m.search(m.query, m.searchGen), m.loadSidebar(), m.loadStatus())
}

// enqueue applies a tag change to the shown message at once (so quick
// successive keys see the new state) and saves it; saves run one at a
// time, in order.
func (m Model) enqueue(req tagReq) (Model, tea.Cmd) {
	if i := slices.IndexFunc(m.msgs, func(x ops.MessageOut) bool { return x.ID == req.msg.ID }); i >= 0 {
		msgs := slices.Clone(m.msgs)
		msgs[i].Tags = applyTagChange(msgs[i].Tags, req.add, req.remove)
		m.msgs = msgs
		if m.open != nil && m.open.Message.ID == req.msg.ID {
			open := *m.open
			open.Message.Tags = msgs[i].Tags
			m.open = &open
		}
	}
	m.tagQueue = append(m.tagQueue, req)
	var cmd tea.Cmd
	if !m.tagBusy {
		m, cmd = m.startNextTag()
	}
	// A message that no longer belongs in the view (archived from the
	// inbox, trashed, restored from the trash) leaves it at once, and the
	// next one is selected (and opened, when reading).
	if m.leavesView(req) {
		var next tea.Cmd
		m, next = m.dropFromView(req.msg.ID)
		cmd = tea.Batch(cmd, next)
	}
	return m, cmd
}

func (m Model) leavesView(req tagReq) bool {
	switch {
	case m.query == queryInbox && slices.Contains(req.remove, ops.TagInbox):
		return true
	case m.query == queryTrash:
		return slices.Contains(req.remove, ops.TagDeleted)
	default:
		// Searches hide the trash unless they ask for it.
		return slices.Contains(req.add, ops.TagDeleted) && !strings.Contains(m.query, ops.TagDeleted)
	}
}

func (m Model) dropFromView(id string) (Model, tea.Cmd) {
	i := slices.IndexFunc(m.msgs, func(x ops.MessageOut) bool { return x.ID == id })
	if i < 0 {
		return m, nil
	}
	m.msgs = slices.Delete(slices.Clone(m.msgs), i, i+1)
	m.cursor = min(m.cursor, max(len(m.msgs)-1, 0))
	if len(m.msgs) == 0 {
		m.open = nil
		m.focus = focusList
		return m, nil
	}
	if m.open != nil && m.open.Message.ID == id {
		if m.focus == focusReader {
			return m.openSelected()
		}
		m.open = nil
		return m.schedulePreview()
	}
	return m, nil
}

func (m Model) startNextTag() (Model, tea.Cmd) {
	req := m.tagQueue[0]
	m.tagQueue = m.tagQueue[1:]
	m.tagBusy = true
	return m, m.runTag(req)
}

func applyTagChange(tags, add, remove []string) []string {
	var out []string
	for _, t := range tags {
		if !slices.Contains(remove, t) {
			out = append(out, t)
		}
	}
	for _, t := range add {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

// schedulePreview loads the selection into the reader pane after a short
// pause, so holding j does not parse every message on the way. Previews
// never mark a message read.
func (m Model) schedulePreview() (Model, tea.Cmd) {
	if !m.wide() && m.focus != focusReader {
		return m, nil
	}
	m.previewSeq++
	return m, previewAfter(m.previewSeq, m.previewWait)
}

func (m Model) openSelected() (Model, tea.Cmd) {
	sel, ok := m.selected()
	if !ok {
		return m, nil
	}
	m.focus = focusReader
	m.showGen++
	cmds := []tea.Cmd{m.show(sel, m.showGen)}
	if m.prefs.ReadOnOpen() && slices.Contains(sel.Tags, ops.TagUnread) {
		next, cmd := m.enqueue(newTagReq(sel, nil, []string{ops.TagUnread}, ""))
		m = next
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m Model) goTo(query string) (Model, tea.Cmd) {
	m.query, m.cursor, m.offset, m.open, m.focus = query, 0, 0, nil, focusList
	m.searchGen++
	return m, m.search(query, m.searchGen)
}

// startSync runs a sync in the background; auto syncs report only news.
func (m Model) startSync(auto bool) (Model, tea.Cmd) {
	if m.syncing {
		m.notice = "already syncing"
		return m, nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.syncing, m.syncAuto, m.syncLines, m.syncCancel = true, auto, nil, cancel
	return m, m.sync(ctx)
}

// requestQuit leaves at once, unless a sync is running: then it is stopped
// at a safe point first (a second request forces the exit).
func (m Model) requestQuit() (Model, tea.Cmd) {
	if !m.syncing || m.quitting {
		return m, tea.Quit
	}
	m.quitting = true
	if m.syncCancel != nil {
		m.syncCancel()
	}
	m.notice = "stopping the sync safely… (press again to force)"
	return m, nil
}

func (m Model) undoLast() (Model, tea.Cmd) {
	if len(m.undo) == 0 {
		m.notice = "nothing to undo"
		return m, nil
	}
	u := m.undo[len(m.undo)-1]
	m.undo = m.undo[:len(m.undo)-1]
	return m, m.revert(u)
}

func (m Model) selected() (ops.MessageOut, bool) {
	if m.cursor < 0 || m.cursor >= len(m.msgs) {
		return ops.MessageOut{}, false
	}
	return m.msgs[m.cursor], true
}

// relayout fits the reader and the list window to the current size and
// state. The reader content is re-wrapped only when the message, its tags
// or the width change, not on every frame.
func (m *Model) relayout() {
	w, h := m.readerW(), m.bodyHeight()
	m.reader.SetWidth(w)
	m.reader.SetHeight(h)
	key, id := "", ""
	if m.open != nil {
		id = m.open.Message.ID
		key = id + "/" + strconv.Itoa(w) + "/" + m.open.BodyFrom + "/" + strings.Join(m.open.Message.Tags, ",")
	}
	if m.showLinks {
		key += "/links"
	}
	if key != m.openKey {
		sameMessage := id != "" && strings.HasPrefix(m.openKey, id+"/")
		if !sameMessage {
			m.showLinks, m.linkPick = false, 0
			key = strings.TrimSuffix(key, "/links")
		}
		// Converting a large HTML body is the costly part: done once here.
		var body string
		m.links = nil
		if m.open != nil {
			body, m.links = m.styledBody(m.open)
		}
		m.openKey = key
		m.reader.SetContent(m.readerContent(body))
		if !sameMessage {
			m.reader.GotoTop()
		}
	}
	m.clampOffset()
}

func (m *Model) clampOffset() {
	h := m.listHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	m.offset = max(m.offset, 0)
}
