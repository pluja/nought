// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"nought/internal/ops"
	"nought/internal/render"
)

func (m Model) key(k tea.KeyPressMsg) (Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		return m.requestQuit()
	}
	if m.inputKind != inputNone {
		return m.inputKey(k)
	}
	if m.help {
		m.help = false
		return m, nil
	}
	if !m.quitting {
		m.notice, m.noticeErr = "", false
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
			m.cursor = 0
			return m.schedulePreview()
		case "i":
			return m.goTo(queryInbox)
		case "a":
			return m.goTo(queryAll)
		case "u":
			return m.goTo(queryUnread)
		case "f":
			return m.goTo(queryFlagged)
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
		return m.askSync()
	case "z":
		return m.undoLast()
	case "r", "R", "a", "f", "c":
		m.notice = "reply and compose arrive with sub-project 4"
		return m, nil
	}

	switch m.focus {
	case focusSidebar:
		return m.sidebarKey(key)
	case focusReader:
		return m.readerKey(key)
	default:
		return m.listKey(key)
	}
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
	}
	return m.messageAction(key)
}

// messageAction handles keys that act on the selected message, in the list
// and in the reader.
func (m Model) messageAction(key string) (Model, tea.Cmd) {
	sel, ok := m.selected()
	if !ok {
		return m, nil
	}
	switch key {
	case "e":
		if !slices.Contains(sel.Tags, ops.TagInbox) {
			m.notice = "not in the inbox"
			return m, nil
		}
		return m.enqueue(newTagReq(sel, nil, []string{ops.TagInbox}, "archived"))
	case "u":
		if slices.Contains(sel.Tags, ops.TagUnread) {
			return m.enqueue(newTagReq(sel, nil, []string{ops.TagUnread}, "marked read"))
		}
		return m.enqueue(newTagReq(sel, []string{ops.TagUnread}, nil, "marked unread"))
	case "s":
		if slices.Contains(sel.Tags, ops.TagFlagged) {
			return m.enqueue(newTagReq(sel, nil, []string{ops.TagFlagged}, "unflagged"))
		}
		return m.enqueue(newTagReq(sel, []string{ops.TagFlagged}, nil, "flagged"))
	case "l", "t":
		return m.startInput(inputTags, "")
	}
	return m, nil
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
		case m.sidebar:
			m.focus = focusSidebar
		}
	case focusReader:
		if m.sidebar {
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
	return m.schedulePreview()
}

// moveOpen moves to the next or previous message and opens it.
func (m Model) moveOpen(delta int) (Model, tea.Cmd) {
	if len(m.msgs) == 0 {
		return m, nil
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.msgs)-1)
	return m.openSelected()
}

// askSync confirms before a sync that may delete mail from servers.
func (m Model) askSync() (Model, tea.Cmd) {
	if m.syncing {
		m.notice = "already syncing"
		return m, nil
	}
	if !m.needsSyncConfirmation() {
		return m.startSync()
	}
	m.inputKind = inputConfirmSync
	return m, nil
}

func (m Model) startInput(kind inputKind, value string) (Model, tea.Cmd) {
	if kind == inputTags {
		if _, ok := m.selected(); !ok {
			return m, nil
		}
	}
	m.inputKind = kind
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.input.SetWidth(max(m.width-25, 5))
	return m, m.input.Focus()
}

func (m Model) inputKey(k tea.KeyPressMsg) (Model, tea.Cmd) {
	if m.inputKind == inputConfirmSync {
		m.inputKind = inputNone
		if strings.EqualFold(k.String(), "y") {
			return m.startSync()
		}
		m.notice = "sync cancelled"
		return m, nil
	}
	switch k.String() {
	case "esc":
		m.inputKind = inputNone
		m.input.Blur()
		return m, nil
	case "enter":
		kind, value := m.inputKind, strings.TrimSpace(m.input.Value())
		m.inputKind = inputNone
		m.input.Blur()
		return m.submit(kind, value)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

func (m Model) submit(kind inputKind, value string) (Model, tea.Cmd) {
	switch kind {
	case inputSearch:
		return m.goTo(value)
	case inputTags:
		sel, ok := m.selected()
		add, remove := parseTagInput(value)
		if !ok || len(add)+len(remove) == 0 {
			return m, nil
		}
		return m.enqueue(newTagReq(sel, add, remove, "tags changed"))
	case inputPalette:
		return m.command(value)
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
		return m.askSync()
	case "search", "s":
		return m.goTo(arg)
	case "inbox":
		return m.goTo(queryInbox)
	case "tag", "t":
		return m.submit(inputTags, arg)
	case "archive":
		return m.messageAction("e")
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
