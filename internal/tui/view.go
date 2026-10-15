// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"nought/internal/ops"
	"nought/internal/render"
)

// Layout constants.
const (
	wideWidth    = 100 // from here, the reader sits beside the list
	sidebarWidth = 20
)

var (
	styleTitle    = lipgloss.NewStyle().Bold(true)
	styleDim      = lipgloss.NewStyle().Faint(true)
	styleUnread   = lipgloss.NewStyle().Bold(true)
	styleSelected = lipgloss.NewStyle().Reverse(true)
	styleFocused  = lipgloss.NewStyle().Underline(true)
	styleWarn     = lipgloss.NewStyle().Bold(true)
)

func (m Model) wide() bool { return m.width >= wideWidth }

func (m Model) showSidebar() bool {
	readerFullScreen := m.focus == focusReader && !m.wide()
	return m.sidebar && m.width >= 60 && !readerFullScreen
}

func (m Model) sidebarW() int {
	if m.showSidebar() {
		return sidebarWidth
	}
	return 0
}

func (m Model) listW() int {
	rest := m.width - m.sidebarW()
	if m.wide() {
		return max(rest*45/100, 40)
	}
	return rest
}

func (m Model) readerW() int {
	if m.wide() {
		return max(m.width-m.sidebarW()-m.listW()-1, 20)
	}
	return m.width
}

// bodyHeight is the height of the main area (between header and footer).
func (m Model) bodyHeight() int {
	h := m.height - 2 // header and status bar
	if m.inputKind != inputNone || m.notice != "" || m.syncing {
		h--
	}
	return max(h, 1)
}

func (m Model) listHeight() int { return m.bodyHeight() }

func (m *Model) layoutReader() {
	m.reader.SetWidth(m.readerW())
	m.reader.SetHeight(m.bodyHeight())
	if m.open != nil {
		m.reader.SetContent(m.readerContent())
	}
}

// View renders the screen.
func (m Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "nought"
	return v
}

func (m Model) render() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	if m.help {
		return m.renderHelp()
	}
	m.layoutReader()
	header := m.renderHeader()
	var main string
	switch {
	case m.focus == focusReader && !m.wide():
		main = m.renderReader(m.width)
	case m.wide():
		main = lipgloss.JoinHorizontal(lipgloss.Top,
			m.renderSidebar(), m.renderList(), m.renderSeparator(), m.renderReader(m.readerW()))
	default:
		main = lipgloss.JoinHorizontal(lipgloss.Top, m.renderSidebar(), m.renderList())
	}
	lines := []string{header, fit(main, m.width, m.bodyHeight())}
	if line := m.renderPromptLine(); line != "" {
		lines = append(lines, line)
	}
	lines = append(lines, m.renderStatus())
	return strings.Join(lines, "\n")
}

func (m Model) renderHeader() string {
	q := m.query
	if q == "" {
		q = "all mail"
	}
	n := strconv.Itoa(len(m.msgs))
	if m.more {
		n += "+"
	}
	return pad(styleTitle.Render("nought")+"  "+render.SanitizeLine(q)+styleDim.Render("  "+n+" messages"), m.width)
}

func (m Model) renderSidebar() string {
	if !m.showSidebar() {
		return ""
	}
	var lines []string
	for i, it := range m.side {
		label := ansi.Truncate(render.SanitizeLine(it.label), sidebarWidth-6, "…")
		count := ""
		switch {
		case it.count >= pageSize:
			count = strconv.Itoa(pageSize) + "+"
		case it.count > 0:
			count = strconv.Itoa(it.count)
		}
		line := pad(" "+label, sidebarWidth-len(count)-1) + count + " "
		if it.query == m.query {
			line = styleTitle.Render(line)
		}
		if m.focus == focusSidebar && i == m.sideIdx {
			line = styleSelected.Render(line)
		}
		lines = append(lines, line)
	}
	return fit(strings.Join(lines, "\n"), sidebarWidth, m.bodyHeight())
}

func (m Model) renderList() string {
	w, h := m.listW(), m.listHeight()
	if len(m.msgs) == 0 {
		return fit(styleDim.Render(" Nothing here. Inbox Zero!"), w, h)
	}
	var lines []string
	for i := m.offset; i < len(m.msgs) && i < m.offset+h; i++ {
		lines = append(lines, m.renderListLine(m.msgs[i], i == m.cursor, w))
	}
	return fit(strings.Join(lines, "\n"), w, h)
}

func (m Model) renderListLine(msg ops.MessageOut, selected bool, w int) string {
	date := ""
	if msg.Date != nil {
		date = shortDate(msg.Date.Local(), m.now())
	}
	from := ansi.Truncate(render.SanitizeLine(senderName(msg.From)), 18, "…")
	line := m.marker(msg) + " " + pad(date, 6) + " " + pad(from, 18) + " " + render.SanitizeLine(msg.Subject)
	line = pad(ansi.Truncate(line, w, "…"), w)
	switch {
	case selected && m.focus == focusList:
		return styleSelected.Render(line)
	case selected:
		return styleFocused.Render(line)
	case hasTag(msg, ops.TagUnread):
		return styleUnread.Render(line)
	}
	return line
}

// marker shows where a message lives: leaving the server soon, kept on the
// server, or local only (blank).
func (m Model) marker(msg ops.MessageOut) string {
	if len(msg.Server) == 0 {
		return "  "
	}
	leaving := false
	for _, l := range msg.Server {
		if l.LeavesAt != nil {
			leaving = true
		}
	}
	switch {
	case m.prefs.ASCII && leaving:
		return "S>"
	case m.prefs.ASCII:
		return "S "
	case leaving:
		return "⏳"
	default:
		return "☁ "
	}
}

func (m Model) renderSeparator() string {
	return fit(strings.Repeat("│\n", m.bodyHeight()), 1, m.bodyHeight())
}

func (m Model) renderReader(w int) string {
	if m.open == nil {
		hint := " Select a message (Enter opens it)."
		if len(m.msgs) == 0 {
			hint = ""
		}
		return fit(styleDim.Render(hint), w, m.bodyHeight())
	}
	return fit(m.reader.View(), w, m.bodyHeight())
}

// readerContent is the open message, fully sanitized; HTML arrives as text.
func (m Model) readerContent() string {
	if m.open == nil {
		return ""
	}
	o := m.open
	w := max(m.readerW()-1, 20)
	wrap := lipgloss.NewStyle().Width(w)
	var b strings.Builder
	field := func(name, value string) {
		b.WriteString(wrap.Render(styleDim.Render(fmt.Sprintf("%-8s", name+":"))+" "+render.SanitizeLine(value)) + "\n")
	}
	field("From", o.Message.From)
	if len(o.Message.To) > 0 {
		field("To", strings.Join(o.Message.To, ", "))
	}
	if o.Message.Date != nil {
		field("Date", o.Message.Date.Local().Format("Mon, 02 Jan 2006 15:04"))
	}
	field("Subject", o.Message.Subject)
	field("Tags", strings.Join(o.Message.Tags, " "))
	for _, line := range locationLines(o.Message, m.now()) {
		b.WriteString(wrap.Render(styleWarn.Render(line)) + "\n")
	}
	for _, a := range o.Attachments {
		field("Attach", fmt.Sprintf("%s (%s, %s)", a.Filename, a.ContentType, humanSize(a.Size)))
	}
	if o.BodyFrom == "text/html" {
		b.WriteString(styleDim.Render("HTML shown as text; nothing was loaded.") + "\n")
	}
	b.WriteString(strings.Repeat("─", w) + "\n")
	b.WriteString(lipgloss.NewStyle().Width(w).Render(render.Sanitize(strings.TrimRight(o.Body, "\n"))))
	return b.String()
}

// locationLines spells out where a message lives.
func locationLines(msg ops.MessageOut, now time.Time) []string {
	if len(msg.Server) == 0 {
		return []string{"Local only: no longer on any server; kept in your archive."}
	}
	var out []string
	for _, l := range msg.Server {
		where := fmt.Sprintf("On server: %s (%s)", render.SanitizeLine(l.Folder), msg.Account)
		switch {
		case l.LeavesAt == nil:
			out = append(out, where+"; stays there (deletion is off for this account).")
		case !l.LeavesAt.After(now):
			out = append(out, where+"; leaves the server at the next sync.")
		default:
			out = append(out, where+"; leaves the server "+until(now, *l.LeavesAt)+".")
		}
	}
	return out
}

func (m Model) renderPromptLine() string {
	switch {
	case m.inputKind == inputSearch:
		return pad("/"+m.input.View(), m.width)
	case m.inputKind == inputTags:
		return pad("tags (+add -remove): "+m.input.View(), m.width)
	case m.inputKind == inputPalette:
		return pad(":"+m.input.View(), m.width)
	case m.syncing:
		line := "syncing…"
		if len(m.syncLines) > 0 {
			line += " " + render.SanitizeLine(m.syncLines[len(m.syncLines)-1])
		}
		return pad(line, m.width)
	case m.notice != "":
		return pad(render.SanitizeLine(m.notice), m.width)
	}
	return ""
}

func (m Model) renderStatus() string {
	var parts []string
	for _, s := range m.status {
		deletion := "deletion off"
		if s.Deletion {
			deletion = "deletion on"
		}
		synced := "never synced"
		if s.LastSynced != nil {
			synced = "synced " + ago(m.now(), *s.LastSynced)
		}
		parts = append(parts, fmt.Sprintf("%s: %s · %s · %d on server", render.SanitizeLine(s.Account), deletion, synced, s.OnServer))
	}
	if len(parts) == 0 {
		parts = append(parts, "no accounts (nou account add)")
	}
	right := "? help"
	left := strings.Join(parts, " │ ")
	return styleDim.Render(pad(ansi.Truncate(left, max(m.width-len(right)-2, 0), "…"), m.width-len(right)) + right)
}

func (m Model) renderHelp() string {
	help := `nought: keys

  j/k, ↓/↑         move              Enter            open (marks read)
  gg / G           first / last      Esc, h           back to the list
  Ctrl-d / Ctrl-u  half page         J / K            next / previous message
  gi ga gu gf      inbox, all, unread, flagged
  /                search            Tab              switch pane
  b                sidebar           :                command (sync, search, tag,
  e                archive                             archive, undo, reindex, quit)
  u                read / unread     S                sync now
  s                flag / unflag     z                undo the last change
  l, t             tags: +add -remove                  ?  help      q  quit

Where messages live:
  ⏳ (S>)  on a server, leaves it at a coming sync (deletion on)
  ☁  (S )  on a server, stays there (deletion off)
  blank    local only: archived, no longer on any server

Archiving (e) only removes the inbox tag in your archive. Nothing in this
interface deletes mail from a server: that happens only during sync, for
accounts with deletion enabled, after each message's local copy is verified.

Reply and compose arrive with sub-project 4.               (any key closes)`
	return fit(help, m.width, m.height)
}

// fit pads or cuts s to exactly w columns and h lines.
func fit(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i, l := range lines {
		lines[i] = pad(ansi.Truncate(l, w, ""), w)
	}
	return strings.Join(lines, "\n")
}

// pad fills s with spaces to w columns.
func pad(s string, w int) string {
	if n := ansi.StringWidth(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

func hasTag(msg ops.MessageOut, tag string) bool {
	for _, t := range msg.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// senderName prefers the display name of "Name <addr>".
func senderName(from string) string {
	if i := strings.Index(from, " <"); i > 0 {
		return strings.Trim(from[:i], `"`)
	}
	return from
}

func shortDate(t, now time.Time) string {
	switch {
	case t.Year() == now.Year() && t.YearDay() == now.YearDay():
		return t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("Jan 02")
	default:
		return t.Format("2006")
	}
}

func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}

func until(now, t time.Time) string {
	d := t.Sub(now)
	switch {
	case d < time.Hour:
		return "within the hour"
	case d < 48*time.Hour:
		return "in " + strconv.Itoa(int(d.Hours())) + " hours"
	default:
		return "in " + strconv.Itoa(int(d.Hours()/24)) + " days"
	}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
