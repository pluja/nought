// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strconv"

	"github.com/charmbracelet/x/ansi"

	"charm.land/lipgloss/v2"
)

// helpGroup is a titled list of keys.
type helpGroup struct {
	title string
	keys  [][2]string // key, what it does
}

func (m Model) helpGroups() (left, right []helpGroup) {
	left = []helpGroup{
		{"Move", [][2]string{
			{"j k ↓ ↑", "move"},
			{"gg G", "first, last"},
			{"Ctrl-d Ctrl-u", "half a page"},
			{"J K", "next, previous message"},
			{"Tab", "switch pane"},
			{"b", "show / hide the sidebar"},
		}},
		{"Go to", [][2]string{
			{"gi ge", "inbox, archive"},
			{"gu gf", "unread, flagged"},
			{"ga gt", "all mail, trash"},
			{"/", "search"},
			{":", "command, e.g. :search :tag :sync"},
		}},
		{"Anywhere", [][2]string{
			{"S", "sync now"},
			{":empty-trash", "empty the trash (asks)"},
			{":reindex", "rebuild the index"},
			{"?", "this help"},
			{"q", "quit"},
		}},
	}
	right = []helpGroup{
		{"Act on a message", [][2]string{
			{"e", "archive: out of the inbox"},
			{"#", "trash / restore"},
			{"D", "delete for good (asks)"},
			{"u", "read / unread"},
			{"s", "flag / unflag"},
			{"t", "tags: +add -remove"},
			{"z", "undo"},
		}},
		{"Read", [][2]string{
			{"Enter", "open (marks it read)"},
			{"Esc", "back to the list"},
			{"o", "open a link (asks first)"},
			{"L", "show / hide its links"},
			{"v", "plain text / HTML version"},
		}},
	}
	return left, right
}

// renderGroups lays out a column of groups, its keys aligned to the
// widest one.
func (m Model) renderGroups(groups []helpGroup) []string {
	kw := 0
	for _, g := range groups {
		for _, k := range g.keys {
			kw = max(kw, ansi.StringWidth(k[0]))
		}
	}
	var lines []string
	for i, g := range groups {
		if i > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, m.st.title.Render(g.title))
		for _, k := range g.keys {
			lines = append(lines, "  "+m.st.accent.Render(pad(k[0], kw))+"  "+k[1])
		}
	}
	return lines
}

func widest(lines []string) int {
	w := 0
	for _, l := range lines {
		w = max(w, ansi.StringWidth(l))
	}
	return w
}

// helpLines is the whole help: keys, what each action does where, and
// what the list's symbols mean.
func (m Model) helpLines() []string {
	left, right := m.helpGroups()
	l, r := m.renderGroups(left), m.renderGroups(right)
	var lines []string
	lw := widest(l) + 4
	if m.width >= 1+lw+widest(r) {
		for i := range max(len(l), len(r)) {
			var a, b string
			if i < len(l) {
				a = l[i]
			}
			if i < len(r) {
				b = r[i]
			}
			lines = append(lines, " "+pad(a, lw)+b)
		}
	} else {
		for _, s := range append(append(l, ""), r...) {
			lines = append(lines, " "+s)
		}
	}

	kept := "for " + strconv.Itoa(m.trashDays) + " days"
	if m.trashDays == 0 {
		kept = "until the next sync"
	}
	archive := "out of the inbox (deletion is off: the server copy stays)."
	if m.deletionOn {
		archive = "out of the inbox; it leaves the server at the next sync (or after keep_days)."
	}
	what := [][2]string{
		{"e archive", archive},
		{"# trash", "the server copy is deleted at the next sync; here it is kept " + kept + "."},
		{"D delete", "deleted from the server at the next sync, then from here."},
		{"t tags", "here only, like read and flagged: never sent to a server."},
	}
	lines = append(lines, "", " "+m.st.title.Render("What happens where"))
	for _, w := range what {
		lines = append(lines, "   "+m.st.accent.Render(pad(w[0], 9))+"  "+w[1])
	}
	lines = append(lines,
		"   On your phone: archive or delete, and nought does the same here at the next sync.",
		"   Sync only ever deletes from servers, and only after the local copy is verified.",
		"", " "+m.st.title.Render("Where a message lives")+m.st.dim.Render("  (every message is in your local archive)"),
		"   "+m.st.server.Render(m.glyph("◐", "S"))+"  on a server too, and stays (deletion off for the account)",
		"   "+m.st.leaving.Render(m.glyph("◕", "-"))+"  on a server too, leaves it soon (deletion on, or you deleted it)",
		"   "+m.st.local.Render(m.glyph("●", "L"))+"  only here",
		"   "+m.st.unreadDot.Render(m.glyph("◆", "*"))+"  unread (read mail is dimmed)   "+
			m.st.flag.Render("★")+" flagged   "+m.st.tag.Render("#tag")+" your tags   "+
			m.st.warn.Render(m.glyph("⌛", "")+"12d")+" deleted for good in 12 days",
		"", " "+m.st.dim.Render("HTML mail is shown as text: nothing is ever loaded from the internet."),
	)
	return lines
}

// renderHelp shows the help from helpTop, with a status line.
func (m Model) renderHelp() string {
	lines := m.helpLines()
	body := max(m.height-2, 1)
	top := min(m.helpTop, max(len(lines)-body, 0))
	end := min(top+body, len(lines))
	status := "any other key closes"
	if len(lines) > body {
		status = "j/k scroll · " + status
	}
	head := m.st.title.Render("nought") + "  " + m.st.bold.Render("keys") + m.st.dim.Render("  "+status)
	view := lipgloss.JoinVertical(lipgloss.Left, append([]string{head, ""}, lines[top:end]...)...)
	return fit(view, m.width, m.height)
}

// helpKey scrolls the help, or closes it.
func (m Model) helpKey(key string) Model {
	body := max(m.height-2, 1)
	last := max(len(m.helpLines())-body, 0)
	switch key {
	case "j", "down":
		m.helpTop = min(m.helpTop+1, last)
	case "k", "up":
		m.helpTop = max(m.helpTop-1, 0)
	case "space", "ctrl+d", "pgdown":
		m.helpTop = min(m.helpTop+body/2, last)
	case "ctrl+u", "pgup":
		m.helpTop = max(m.helpTop-body/2, 0)
	default:
		m.help, m.helpTop = false, 0
	}
	return m
}
