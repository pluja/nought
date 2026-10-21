// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"slices"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/pluja/nought/internal/ops"
	"github.com/pluja/nought/internal/render"
)

func (m Model) key(k tea.KeyPressMsg) (Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		return m.requestQuit()
	}
	if m.linkPick > 0 {
		return m.linkKey(key)
	}
	if m.inputKind != inputNone {
		return m.inputKey(k)
	}
	if m.help {
		return m.helpKey(k.String()), nil
	}
	if !m.quitting {
		m.notice, m.noticeErr = "", false
	}
	if m.composing != nil {
		return m.reviewKey(key)
	}

	// Two-key sequences starting with g. An unknown second key is handled
	// as if g had not been pressed.
	if m.pendingG {
		m.pendingG = false
		switch key {
		case "g":
			if m.focus == focusReader {
				m.reader.GotoTop()
				return m, nil
			}
			m.cursor, m.draftIdx = 0, 0
			return m.markRange().schedulePreview()
		case "i":
			return m.goTo(queryInbox)
		case "a":
			return m.goTo(queryAll)
		case "u":
			return m.goTo(queryUnread)
		case "f":
			return m.goTo(queryFlagged)
		case "t":
			return m.goTo(queryTrash)
		case "e":
			return m.goTo(queryArchive)
		case "d":
			return m.goTo(queryDrafts)
		}
	}

	switch key {
	case "q":
		if m.focus == focusReader && !m.wide() {
			m.focus = focusList
			return m, nil
		}
		return m.requestQuit()
	case "?":
		m.help = true
		return m, nil
	case "g":
		m.pendingG = true
		return m, nil
	case "b":
		m.sidebar = !m.sidebar
		if !m.sidebar && m.focus == focusSidebar {
			m.focus = focusList
		}
		return m, nil
	case "tab":
		return m.cycleFocus(), nil
	case "/":
		return m.startInput(inputSearch, "")
	case ":":
		return m.startInput(inputPalette, "")
	case "S":
		return m.startSync(false)
	case "z":
		if m.sendingID != "" {
			return m, m.cancelSend(m.sendingID)
		}
		return m.undoLast()
	case "o":
		return m.startLinkPick()
	case "v":
		return m.switchBody()
	case "L":
		if m.open == nil || len(m.links) == 0 {
			m.notice = "no links in this message"
			return m, nil
		}
		m.showLinks = !m.showLinks
		return m, nil
	case "r", "R", "f", "c":
		return m.startDraft(key)
	}

	switch m.focus {
	case focusSidebar:
		return m.sidebarKey(key)
	case focusReader:
		return m.readerKey(key)
	}
	if m.query == queryDrafts {
		return m.draftsKey(key)
	}
	return m.listKey(key)
}

func (m Model) listKey(key string) (Model, tea.Cmd) {
	switch key {
	case "j", "down":
		return m.move(1)
	case "k", "up":
		return m.move(-1)
	case "ctrl+d", "pgdown":
		return m.move(max(m.listHeight()/2, 1))
	case "ctrl+u", "pgup":
		return m.move(-max(m.listHeight()/2, 1))
	case "G", "end":
		return m.move(len(m.msgs))
	case "home":
		return m.move(-len(m.msgs))
	case "enter", "right":
		return m.openSelected()
	case "x":
		return m.toggleMark()
	case "V":
		return m.toggleRange()
	case "*":
		return m.markAll()
	case "esc":
		return m.escMarks(), nil
	}
	return m.messageAction(key)
}

// messageAction handles keys that act on the selected message, in the list
// and in the reader, or on the marked messages when there are any.
func (m Model) messageAction(key string) (Model, tea.Cmd) {
	if m.markingAll && m.focus != focusReader {
		m.notice = "marking… (a moment)"
		return m, nil
	}
	msgs, ok := m.actionTargets()
	if !ok {
		return m, nil
	}
	switch key {
	case "e":
		return m.archive(msgs)
	case "u": // any unread: all read; else all unread
		if slices.ContainsFunc(msgs, hasTag(ops.TagUnread)) {
			return m.enqueueAction(newTagReq(msgs, nil, []string{ops.TagUnread}, "marked read"))
		}
		return m.enqueueAction(newTagReq(msgs, []string{ops.TagUnread}, nil, "marked unread"))
	case "s": // all flagged: unflag them; else flag all
		if all(msgs, hasTag(ops.TagFlagged)) {
			return m.enqueueAction(newTagReq(msgs, nil, []string{ops.TagFlagged}, "unflagged"))
		}
		return m.enqueueAction(newTagReq(msgs, []string{ops.TagFlagged}, nil, "flagged"))
	case "l", "t":
		return m.startInput(inputTags, "")
	case "#":
		req, err := m.trashReq(msgs)
		if err != nil {
			m.notice, m.noticeErr = err.Error(), true
			return m, nil
		}
		return m.enqueueAction(req)
	case "D":
		m.deleting = msgs
		m.inputKind = inputConfirmDelete
		return m, nil
	case "w":
		switch {
		case len(m.marked) > 0:
			m.notice = "attachments are saved from one message (Esc clears the marks)"
		case m.open == nil || m.open.Message.ID != msgs[0].ID:
			m.notice = "open the message first (Enter)"
		case len(m.open.Attachments) == 0:
			m.notice = "no attachments in this message"
		default:
			return m.startInput(inputSaveDir, defaultSaveDir())
		}
		return m, nil
	}
	return m, nil
}

// archive takes the messages still in the inbox out of it.
func (m Model) archive(msgs []ops.MessageOut) (Model, tea.Cmd) {
	inbox := slices.DeleteFunc(slices.Clone(msgs), func(msg ops.MessageOut) bool { return !hasTag(ops.TagInbox)(msg) })
	if len(inbox) == 0 {
		m.notice = "not in the inbox"
		return m, nil
	}
	req := newTagReq(inbox, nil, []string{ops.TagInbox}, "archived")
	for _, msg := range inbox {
		if len(msg.Server) > 0 && m.deletionOff(msg.Account) && !m.deletionNoted[msg.Account] {
			req.note = "deletion is off for " + msg.Account + ", so it stays on the server (and other devices)"
			req.noteAccount = msg.Account
			break
		}
	}
	return m.enqueueAction(req)
}

func (m Model) readerKey(key string) (Model, tea.Cmd) {
	switch key {
	case "esc", "h", "left":
		m.focus = focusList
		return m, nil
	case "J":
		return m.moveOpen(1)
	case "K":
		return m.moveOpen(-1)
	case "j", "down":
		m.reader.ScrollDown(1)
		return m, nil
	case "k", "up":
		m.reader.ScrollUp(1)
		return m, nil
	case "space", "ctrl+d", "pgdown":
		m.reader.ScrollDown(max(m.reader.Height()/2, 1))
		return m, nil
	case "ctrl+u", "pgup":
		m.reader.ScrollUp(max(m.reader.Height()/2, 1))
		return m, nil
	case "G", "end":
		m.reader.GotoBottom()
		return m, nil
	}
	return m.messageAction(key)
}

func (m Model) sidebarKey(key string) (Model, tea.Cmd) {
	switch key {
	case "j", "down":
		m.sideIdx = min(m.sideIdx+1, max(len(m.side)-1, 0))
	case "k", "up":
		m.sideIdx = max(m.sideIdx-1, 0)
	case "enter", "l", "right":
		if m.sideIdx < len(m.side) {
			return m.goTo(m.side[m.sideIdx].query)
		}
	case "esc", "h", "left":
		m.focus = focusList
	}
	return m, nil
}

func (m Model) cycleFocus() Model {
	switch m.focus {
	case focusList:
		switch {
		case m.wide() && m.open != nil:
			m.focus = focusReader
		case m.showSidebar():
			m.focus = focusSidebar
		}
	case focusReader:
		if m.showSidebar() {
			m.focus = focusSidebar
		} else {
			m.focus = focusList
		}
	default:
		m.focus = focusList
	}
	return m
}

func (m Model) move(delta int) (Model, tea.Cmd) {
	if len(m.msgs) == 0 {
		return m, nil
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.msgs)-1)
	return m.markRange().schedulePreview()
}

// moveOpen moves to the next or previous message and opens it.
func (m Model) moveOpen(delta int) (Model, tea.Cmd) {
	if len(m.msgs) == 0 {
		return m, nil
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.msgs)-1)
	return m.openSelected()
}

func (m Model) startInput(kind inputKind, value string) (Model, tea.Cmd) {
	if kind == inputTags || kind == inputSaveDir {
		// The prompt acts on this message, whatever a refresh does to the
		// list meanwhile.
		targets, ok := m.actionTargets()
		if !ok {
			return m, nil
		}
		m.inputTargets = targets
	}
	m.inputKind = kind
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.input.SetWidth(max(m.width-25, 5))
	if kind == inputTo { // room for the suggestions
		m.input.SetWidth(max(m.width/2, 10))
	}
	return m, m.input.Focus()
}

// confirmKey answers a y/N question: any key but y is a no.
func (m Model) confirmKey(k tea.KeyPressMsg) (Model, tea.Cmd) {
	kind, yes := m.inputKind, k.String() == "y" || k.String() == "Y"
	m.inputKind = inputNone
	switch {
	case kind == inputConfirmEmptyTrash && yes:
		m.notice = "emptying the trash…"
		return m, m.emptyTrash()
	case kind == inputConfirmEmptyTrash:
		m.notice = "trash kept"
	case kind == inputConfirmDiscard && yes:
		return m, m.discard(m.discarding)
	case kind == inputConfirmDiscard:
		m.notice = "draft kept"
	case yes: // inputConfirmDelete
		msgs := m.deleting
		m, drop := m.clearMarks().dropFromView(msgs...)
		return m, tea.Batch(drop, m.deleteNow(msgs))
	default:
		m.notice = "not deleted"
	}
	return m, nil
}

func (m Model) inputKey(k tea.KeyPressMsg) (Model, tea.Cmd) {
	switch m.inputKind {
	case inputConfirmEmptyTrash, inputConfirmDiscard, inputConfirmDelete:
		return m.confirmKey(k)
	}
	if (m.inputKind == inputTo || m.inputKind == inputAttach) && m.composing == nil {
		m.inputKind = inputNone // the review screen closed (sent) under it
		m.input.Blur()
		return m, nil
	}
	switch k.String() {
	case "esc":
		m.inputKind = inputNone
		m.input.Blur()
		m.suggestions = nil
		return m, nil
	case "tab":
		if m.inputKind == inputTo {
			return m.complete(), nil
		}
	case "enter":
		kind, value := m.inputKind, strings.TrimSpace(m.input.Value())
		m.inputKind = inputNone
		m.input.Blur()
		return m.submit(kind, value)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	if m.inputKind == inputTo {
		m.suggestions = nil
		if token := lastAddress(m.input.Value()); token != "" {
			cmd = tea.Batch(cmd, m.suggest(m.composing.Account, token))
		}
	}
	if m.inputKind == inputLinkNumber && linkNumberComplete(m.input.Value(), len(m.links)) {
		value := m.input.Value()
		m.inputKind = inputNone
		m.input.Blur()
		return m.pickLink(value)
	}
	return m, cmd
}

// linkNumberComplete reports whether a typed link number cannot grow into
// another valid one ("3" with 25 links), so no Enter is needed.
func linkNumberComplete(typed string, links int) bool {
	n, err := strconv.Atoi(typed)
	if err != nil || n < 1 {
		return false
	}
	return n*10 > links
}

func (m Model) submit(kind inputKind, value string) (Model, tea.Cmd) {
	switch kind {
	case inputSearch:
		return m.goTo(value)
	case inputTags:
		add, remove := parseTagInput(value)
		if len(add)+len(remove) == 0 {
			return m, nil
		}
		return m.enqueueAction(newTagReq(m.inputTargets, add, remove, "tags changed"))
	case inputPalette:
		return m.command(value)
	case inputLinkNumber:
		return m.pickLink(value)
	case inputTo:
		m.suggestions = nil
		if value = strings.TrimSuffix(value, ","); value != "" {
			return m, m.setDraft(ops.DraftSetIn{ID: m.composing.ID, To: value})
		}
	case inputSaveDir:
		if value != "" {
			return m, m.saveAttachments(m.inputTargets[0], value)
		}
	case inputAttach:
		if value != "" {
			return m, m.setDraft(ops.DraftSetIn{ID: m.composing.ID, Attach: []string{value}})
		}
	}
	return m, nil
}

// command runs a ":" palette command.
func (m Model) command(line string) (Model, tea.Cmd) {
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch name {
	case "q", "quit":
		return m.requestQuit()
	case "sync":
		m.syncAccept = arg == "accept-removals"
		return m.startSync(false)
	case "search", "s":
		return m.goTo(arg)
	case "inbox":
		return m.goTo(queryInbox)
	case "tag", "t":
		// Like t: the marked messages, or the selected one, as of now.
		targets, ok := m.actionTargets()
		if !ok {
			m.notice = "no message to tag"
			return m, nil
		}
		m.inputTargets = targets
		return m.submit(inputTags, arg)
	case "archive":
		return m.messageAction("e")
	case "trash":
		return m.goTo(queryTrash)
	case "drafts":
		return m.goTo(queryDrafts)
	case "compose":
		return m.startDraft("c")
	case "empty-trash":
		m.inputKind = inputConfirmEmptyTrash
		return m, nil
	case "undo":
		return m.undoLast()
	case "reindex":
		m.notice = "reindexing…"
		return m, m.reindex()
	case "help", "h":
		m.help = true
		return m, nil
	case "":
		return m, nil
	}
	m.notice = "unknown command: " + render.SanitizeLine(name) + " (try :help)"
	return m, nil
}

// switchBody shows the other version of a message that has both (plain
// text, or HTML converted to text), and keeps that choice for the session.
func (m Model) switchBody() (Model, tea.Cmd) {
	sel, ok := m.selected()
	if m.open == nil || !ok || m.open.Alternative == "" {
		m.notice = "this message has only one version"
		return m, nil
	}
	prefer := "plain"
	if m.open.Alternative == "text/html" {
		prefer = "html"
	}
	m.showGen++
	return m, m.showVersion(sel, m.showGen, prefer)
}

// deletionOff reports that an account is known to have deletion off; an
// account whose status is not loaded is not.
func (m Model) deletionOff(account string) bool {
	return slices.ContainsFunc(m.status, func(s ops.AccountStatusOut) bool { return s.Account == account && !s.Deletion })
}
