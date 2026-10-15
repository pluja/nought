// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"nought/internal/ops"
	"nought/internal/render"
)

// maxUndo bounds the undo history.
const maxUndo = 50

// Update handles one event.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layoutReader()
		return m, nil

	case searchDoneMsg:
		if msg.query != m.query {
			return m, nil // a newer view was requested meanwhile
		}
		if msg.err != nil {
			m.notice = "search: " + msg.err.Error()
			return m, nil
		}
		m.msgs, m.more = msg.out.Messages, msg.out.More
		m.cursor = min(m.cursor, max(len(m.msgs)-1, 0))
		m.clampOffset()
		return m, m.preview()

	case showDoneMsg:
		if sel, ok := m.selected(); !ok || sel.ID != msg.id {
			return m, nil // the selection moved on
		}
		if msg.err != nil {
			m.notice = msg.err.Error()
			return m, nil
		}
		m.open = &msg.out
		m.reader.SetContent(m.readerContent())
		m.reader.GotoTop()
		return m, nil

	case tagDoneMsg:
		if msg.err != nil {
			m.notice = msg.err.Error()
		} else if msg.notice != "" {
			m.notice = msg.notice
		}
		if msg.undo != nil {
			m.undo = append(m.undo, *msg.undo)
			if len(m.undo) > maxUndo {
				m.undo = m.undo[1:]
			}
		}
		return m, tea.Batch(m.search(m.query), m.loadSidebar())

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
		m.syncing, m.syncLines = false, nil
		m.notice = syncSummary(msg.out, msg.err)
		return m, tea.Batch(m.search(m.query), m.loadStatus(), m.loadSidebar())

	case reindexDoneMsg:
		if msg.err != nil {
			m.notice = "reindex: " + msg.err.Error()
		} else {
			m.notice = "reindexed " + itoa(msg.out.Messages) + " messages"
		}
		return m, tea.Batch(m.search(m.query), m.loadSidebar())

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

func (m Model) key(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	if key == "ctrl+c" {
		return m, tea.Quit
	}
	if m.inputKind != inputNone {
		return m.inputKey(k)
	}
	if m.help {
		m.help = false
		return m, nil
	}
	m.notice = ""

	// Two-key sequences starting with g.
	if m.pendingG {
		m.pendingG = false
		switch key {
		case "g":
			m.cursor = 0
			m.clampOffset()
			return m, m.preview()
		case "i":
			return m.goTo(queryInbox)
		case "a":
			return m.goTo(queryAll)
		case "u":
			return m.goTo(queryUnread)
		case "f":
			return m.goTo(queryFlagged)
		}
		return m, nil
	}

	switch key {
	case "q":
		if m.focus == focusReader && !m.wide() {
			m.focus = focusList
			return m, nil
		}
		return m, tea.Quit
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
		m.layoutReader()
		return m, nil
	case "tab":
		return m.cycleFocus(), nil
	case "/":
		return m.startInput(inputSearch, "")
	case ":":
		return m.startInput(inputPalette, "")
	case "S":
		return m.startSync()
	case "z":
		return m.undoLast()
	case "r", "R", "f", "c":
		m.notice = "reply and compose arrive with sub-project 4"
		return m, nil
	}

	switch m.focus {
	case focusSidebar:
		return m.sidebarKey(key)
	case focusReader:
		return m.readerKey(k)
	default:
		return m.listKey(key)
	}
}

func (m Model) listKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "j", "down":
		return m.move(1)
	case "k", "up":
		return m.move(-1)
	case "ctrl+d", "pgdown":
		return m.move(m.listHeight() / 2)
	case "ctrl+u", "pgup":
		return m.move(-m.listHeight() / 2)
	case "G", "end":
		return m.move(len(m.msgs))
	case "home":
		return m.move(-len(m.msgs))
	case "enter", "right":
		return m.openSelected()
	case "l":
		return m.startInput(inputTags, "")
	}
	return m.messageAction(key)
}

// messageAction handles keys that act on the selected message, in the list
// and in the reader.
func (m Model) messageAction(key string) (tea.Model, tea.Cmd) {
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
		return m.change(sel, nil, []string{ops.TagInbox}, "archived")
	case "u":
		if slices.Contains(sel.Tags, ops.TagUnread) {
			return m.change(sel, nil, []string{ops.TagUnread}, "marked read")
		}
		return m.change(sel, []string{ops.TagUnread}, nil, "marked unread")
	case "s":
		if slices.Contains(sel.Tags, ops.TagFlagged) {
			return m.change(sel, nil, []string{ops.TagFlagged}, "unflagged")
		}
		return m.change(sel, []string{ops.TagFlagged}, nil, "flagged")
	case "t":
		return m.startInput(inputTags, "")
	}
	return m, nil
}

// change applies a tag change to the shown message at once (so quick
// successive keys see the new state) and saves it in the background; the
// view is refreshed from the archive when the change is done.
func (m Model) change(sel ops.MessageOut, add, remove []string, what string) (tea.Model, tea.Cmd) {
	cmd := m.tag(sel, add, remove, what)
	if m.cursor < len(m.msgs) && m.msgs[m.cursor].ID == sel.ID {
		msgs := slices.Clone(m.msgs)
		var tags []string
		for _, t := range sel.Tags {
			if !slices.Contains(remove, t) {
				tags = append(tags, t)
			}
		}
		for _, t := range add {
			if !slices.Contains(tags, t) {
				tags = append(tags, t)
			}
		}
		slices.Sort(tags)
		msgs[m.cursor].Tags = tags
		m.msgs = msgs
	}
	return m, cmd
}

func (m Model) readerKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
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
	case "l":
		return m.startInput(inputTags, "")
	}
	return m.messageAction(k.String())
}

func (m Model) sidebarKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "j", "down":
		m.sideIdx = min(m.sideIdx+1, max(len(m.side)-1, 0))
	case "k", "up":
		m.sideIdx = max(m.sideIdx-1, 0)
	case "enter", "l", "right":
		if m.sideIdx < len(m.side) {
			m.focus = focusList
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

func (m Model) move(delta int) (tea.Model, tea.Cmd) {
	if len(m.msgs) == 0 {
		return m, nil
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.msgs)-1)
	m.clampOffset()
	return m, m.preview()
}

// moveOpen moves to the next or previous message and opens it.
func (m Model) moveOpen(delta int) (tea.Model, tea.Cmd) {
	next, _ := m.move(delta)
	return next.(Model).openSelected()
}

// preview loads the selected message into the reader pane (wide layouts).
// Previewing never marks a message read.
func (m Model) preview() tea.Cmd {
	sel, ok := m.selected()
	if !ok {
		return nil
	}
	if !m.wide() && m.focus != focusReader {
		return nil
	}
	return m.show(sel)
}

func (m Model) openSelected() (tea.Model, tea.Cmd) {
	sel, ok := m.selected()
	if !ok {
		return m, nil
	}
	m.focus = focusReader
	cmds := []tea.Cmd{m.show(sel)}
	if m.prefs.ReadOnOpen() && slices.Contains(sel.Tags, ops.TagUnread) {
		next, cmd := m.change(sel, nil, []string{ops.TagUnread}, "")
		m = next.(Model)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m Model) goTo(query string) (tea.Model, tea.Cmd) {
	m.query, m.cursor, m.offset, m.open = query, 0, 0, nil
	return m, m.search(query)
}

func (m Model) startSync() (tea.Model, tea.Cmd) {
	if m.syncing {
		m.notice = "already syncing"
		return m, nil
	}
	m.syncing, m.syncLines = true, nil
	return m, m.sync()
}

func (m Model) undoLast() (tea.Model, tea.Cmd) {
	if len(m.undo) == 0 {
		m.notice = "nothing to undo"
		return m, nil
	}
	u := m.undo[len(m.undo)-1]
	m.undo = m.undo[:len(m.undo)-1]
	return m, m.revert(u)
}

func (m Model) startInput(kind inputKind, value string) (tea.Model, tea.Cmd) {
	if kind == inputTags {
		if _, ok := m.selected(); !ok {
			return m, nil
		}
	}
	m.inputKind = kind
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.input.SetWidth(max(m.width-20, 10))
	return m, m.input.Focus()
}

func (m Model) inputKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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

func (m Model) submit(kind inputKind, value string) (tea.Model, tea.Cmd) {
	switch kind {
	case inputSearch:
		m.focus = focusList
		return m.goTo(value)
	case inputTags:
		sel, ok := m.selected()
		add, remove := parseTagInput(value)
		if !ok || len(add)+len(remove) == 0 {
			return m, nil
		}
		return m.change(sel, add, remove, "tags changed")
	case inputPalette:
		return m.command(value)
	}
	return m, nil
}

// command runs a ":" palette command.
func (m Model) command(line string) (tea.Model, tea.Cmd) {
	name, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch name {
	case "q", "quit":
		return m, tea.Quit
	case "sync":
		return m.startSync()
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

func (m Model) selected() (ops.MessageOut, bool) {
	if m.cursor < 0 || m.cursor >= len(m.msgs) {
		return ops.MessageOut{}, false
	}
	return m.msgs[m.cursor], true
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
