// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/pluja/nought/internal/ops"
	"github.com/pluja/nought/internal/render"
)

// Layout constants.
const (
	wideWidth    = 100 // from here, the reader sits beside the list
	sidebarWidth = 20
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
		return max(rest*55/100, 40)
	}
	return rest
}

func (m Model) readerW() int {
	if m.wide() {
		return max(m.width-m.sidebarW()-m.listW()-1, 20)
	}
	return m.width
}

// compact is a terminal too short for a header and a status bar.
func (m Model) compact() bool { return m.height < 4 }

// bodyHeight is the height of the main area (between header and footer).
func (m Model) bodyHeight() int {
	if m.compact() {
		return max(m.height, 1)
	}
	h := m.height - 2 // header and status bar
	if m.renderPromptLine() != "" {
		h--
	}
	if m.hintsShown() {
		h--
	}
	return max(h, 1)
}

// hintsShown: the key hints line needs a little room.
func (m Model) hintsShown() bool { return m.prefs.ShowHints() && m.height >= 10 }

func (m Model) listHeight() int { return m.bodyHeight() }

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
	if m.linkPick > 0 && m.linkPick <= len(m.links) {
		return overlay(m.renderScreen(), m.renderLinkBox(), m.width, m.height)
	}
	return m.renderScreen()
}

func (m Model) renderScreen() string {
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
	if m.compact() {
		return fit(main, m.width, m.height)
	}
	lines := []string{header, fit(main, m.width, m.bodyHeight())}
	if line := m.renderPromptLine(); line != "" {
		lines = append(lines, line)
	}
	lines = append(lines, m.renderStatus())
	if m.hintsShown() {
		lines = append(lines, m.renderHints())
	}
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
	head := m.st.title.Render("nought") + "  " + m.st.bold.Render(render.SanitizeLine(q)) + m.st.dim.Render("  "+n+" messages")
	if m.query == queryTrash {
		// The trash explains itself: when its mail goes, and how to act.
		when := "at the next sync"
		if m.trashDays > 0 {
			when = strconv.Itoa(m.trashDays) + " days after deletion"
		}
		head = m.st.title.Render("nought") + "  " + m.st.bold.Render("Trash") + m.st.dim.Render("  "+n+" messages") +
			m.st.warn.Render("  · deleted for good "+when) + m.st.dim.Render(" · # restores · :empty-trash")
	}
	return fit(head, m.width, 1)
}

// countdown is how long until a message is deleted for good (trash or an
// expiring rule), or "" if it is not.
func (m Model) countdown(msg ops.MessageOut) string {
	at, ok := ops.ExpiresAt(msg.Tags)
	if !ok {
		return ""
	}
	days := int(math.Ceil(at.Sub(m.now()).Hours() / 24))
	left := strconv.Itoa(days) + "d"
	if days <= 0 {
		left = "today"
	}
	return m.glyph("⌛", "") + left
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
		countStyle := m.st.dim
		switch it.query {
		case queryInbox:
			countStyle = m.st.warn.Bold(true)
		case queryUnread:
			countStyle = m.st.unreadDot
		}
		labelStyle := m.st.plain
		if strings.HasPrefix(it.label, "#") {
			labelStyle = m.st.tag
		}
		if it.query == m.query {
			labelStyle = m.st.title
		}
		line := labelStyle.Render(pad(" "+label, sidebarWidth-len(count)-1)) + countStyle.Render(count) + " "
		if m.focus == focusSidebar && i == m.sideIdx {
			line = m.st.accent.Render("▌") + ansi.Cut(line, 1, sidebarWidth)
		}
		if it.heading != "" {
			lines = append(lines, "", m.st.dim.Render(" "+it.heading))
		}
		lines = append(lines, line)
	}
	return fit(strings.Join(lines, "\n"), sidebarWidth, m.bodyHeight())
}

func (m Model) renderList() string {
	w, h := m.listW(), m.listHeight()
	if len(m.msgs) == 0 {
		return fit(m.st.dim.Render(" Nothing here. Inbox Zero!"), w, h)
	}
	var lines []string
	for i := m.offset; i < len(m.msgs) && i < m.offset+h; i++ {
		lines = append(lines, m.renderListLine(m.msgs[i], i == m.cursor, w))
	}
	return fit(strings.Join(lines, "\n"), w, h)
}

func (m Model) renderListLine(msg ops.MessageOut, selected bool, w int) string {
	highlight := selected && m.focus == focusList
	st := func(s lipgloss.Style) lipgloss.Style {
		if highlight {
			return s.Background(m.st.selBG)
		}
		return s
	}
	// Read mail recedes; unread mail stands out at full brightness. Bold
	// alone is too subtle in many terminal fonts.
	unread := slices.Contains(msg.Tags, ops.TagUnread)
	text := m.st.dim
	if unread {
		text = m.st.bold
	}
	gutter := st(m.st.plain).Render(" ")
	if selected {
		gutter = st(m.st.accent).Render("▌")
	}
	state, stateStyle := " ", m.st.plain
	switch {
	case slices.Contains(msg.Tags, ops.TagFlagged):
		state, stateStyle = "★", m.st.flag
	case unread:
		state, stateStyle = m.glyph("◆", "*"), m.st.unreadDot
	}
	date := ""
	if msg.Date != nil {
		date = shortDate(msg.Date.Local(), m.now())
	}
	from := ansi.Truncate(render.SanitizeLine(senderName(msg.From)), 22, "…")
	glyph, where := m.location(msg)
	sp := st(m.st.plain).Render(" ")
	line := gutter + st(where).Render(glyph) + sp + st(stateStyle).Render(state) + sp +
		st(m.st.dim).Render(pad(date, 6)) + sp + st(text).Render(pad(from, 22)) + sp +
		st(text).Render(render.SanitizeLine(msg.Subject))
	if tags := userTags(msg.Tags); len(tags) > 0 {
		line += sp + st(m.st.tag).Render(render.SanitizeLine("#"+strings.Join(tags, " #")))
	}
	// A deletion date stays visible at the right edge, whatever the subject.
	left := m.countdown(msg)
	room := w
	if left != "" {
		room = max(w-ansi.StringWidth(left)-1, 0)
	}
	line = ansi.Truncate(line, room, "…")
	if n := ansi.StringWidth(line); n < room {
		line += st(m.st.plain).Render(strings.Repeat(" ", room-n))
	}
	if left != "" && room > 0 {
		line += sp + st(m.st.warn).Render(left)
	}
	return line
}

// userTags are the tags worth showing in the list: not the ones already
// shown as symbols or implied by the view.
func userTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		switch {
		case t == ops.TagInbox, t == ops.TagUnread, t == ops.TagFlagged, t == ops.TagReplied,
			strings.HasPrefix(t, "folder:"), hiddenTag(t):
			continue
		}
		out = append(out, t)
	}
	return out
}

// hiddenTag: the trash and expiry tags are shown as a sentence in the
// reader instead.
func hiddenTag(t string) bool { return ops.IsDeletionTag(t) }

// expiryLine says when a deleted or expiring message is deleted for good.
func expiryLine(tags []string, now time.Time) string {
	at, ok := ops.ExpiresAt(tags)
	switch {
	case !ok:
		if slices.Contains(tags, ops.TagDeleted) {
			return "In the trash"
		}
		return ""
	case !at.After(now):
		return "Deleted for good at the next sync"
	case slices.Contains(tags, ops.TagDeleted):
		return "In the trash until " + at.Format("2 Jan 2006") + " (# restores)"
	default:
		return "Expires on " + at.Format("2 Jan 2006") + ", then deleted for good"
	}
}

// location tells where a message lives, as a circle that fills up as the
// message becomes yours alone: ◐ also on a server (stays), ◕ leaving the
// server soon, ● only in the local archive.
func (m Model) location(msg ops.MessageOut) (string, lipgloss.Style) {
	leaving := false
	for _, l := range msg.Server {
		if l.LeavesAt != nil {
			leaving = true
		}
	}
	switch {
	case len(msg.Server) == 0:
		return m.glyph("●", "L"), m.st.local
	case leaving:
		return m.glyph("◕", "-"), m.st.leaving
	default:
		return m.glyph("◐", "S"), m.st.server
	}
}

func (m Model) glyph(unicode, ascii string) string {
	if m.prefs.ASCII {
		return ascii
	}
	return unicode
}

// marker is the location glyph alone (for tests and plain output).
func (m Model) marker(msg ops.MessageOut) string {
	g, _ := m.location(msg)
	return g
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
		return fit(m.st.dim.Render(hint), w, m.bodyHeight())
	}
	return fit(m.reader.View(), w, m.bodyHeight())
}

// readerContent is the open message with its styled body, fully
// sanitized; HTML arrives as text.
func (m Model) readerContent(body string) string {
	if m.open == nil {
		return ""
	}
	o := m.open
	w := max(m.readerW()-1, 1)
	wrap := lipgloss.NewStyle().Width(w)
	var b strings.Builder
	field := func(name, value string) {
		b.WriteString(wrap.Render(m.st.dim.Render(fmt.Sprintf("%-8s", name+":"))+" "+render.SanitizeLine(value)) + "\n")
	}
	field("From", o.Message.From)
	if len(o.Message.To) > 0 {
		field("To", strings.Join(o.Message.To, ", "))
	}
	if o.Message.Date != nil {
		field("Date", o.Message.Date.Local().Format("Mon, 02 Jan 2006 15:04"))
	}
	b.WriteString(wrap.Render(m.st.dim.Render("Subject:")+" "+m.st.bold.Render(render.SanitizeLine(o.Message.Subject))) + "\n")
	if tags := userTags(o.Message.Tags); len(tags) > 0 {
		b.WriteString(wrap.Render(m.st.dim.Render("Tags:   ")+" "+m.st.tag.Render(render.SanitizeLine("#"+strings.Join(tags, " #")))) + "\n")
	}
	for _, a := range o.Attachments {
		field("Attach", fmt.Sprintf("%s (%s)", a.Filename, humanSize(a.Size)))
	}
	if line := expiryLine(o.Message.Tags, m.now()); line != "" {
		b.WriteString(wrap.Render(m.st.warn.Render(line)) + "\n")
	}
	glyph, where := m.location(o.Message)
	for _, line := range locationLines(o.Message, m.now()) {
		b.WriteString(wrap.Render(where.Render(glyph+" "+line)) + "\n")
	}
	b.WriteString(m.st.dim.Render(strings.Repeat("─", w)) + "\n")
	b.WriteString(wrap.Render(body))
	b.WriteString(m.linksFooter(w))
	return b.String()
}

// locationLines says briefly where a message lives (details are in ?).
func locationLines(msg ops.MessageOut, now time.Time) []string {
	if len(msg.Server) == 0 {
		return []string{"Local only"}
	}
	var out []string
	for _, l := range msg.Server {
		where := render.SanitizeLine(l.Folder) + " on " + msg.Account
		switch {
		case l.LeavesAt == nil:
			out = append(out, "On server · "+where)
		case !l.LeavesAt.After(now):
			out = append(out, "Leaves the server at the next sync · "+where)
		default:
			out = append(out, "Leaves the server "+until(now, *l.LeavesAt)+" · "+where)
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
	case m.inputKind == inputLinkNumber:
		return pad("open link number (1-"+strconv.Itoa(len(m.links))+"): "+m.input.View(), m.width)
	case m.inputKind == inputConfirmEmptyTrash:
		return fit(m.st.errText.Render("Delete everything in the trash for good, here and on servers? (y/N)"), m.width, 1)
	case m.inputKind == inputConfirmDelete:
		q := "Delete permanently, here and on the server? (y/N) "
		return fit(m.st.errText.Render(q)+render.SanitizeLine(m.deleting.Subject), m.width, 1)
	case m.syncing:
		line := "syncing…"
		if len(m.syncLines) > 0 {
			line += " " + render.SanitizeLine(m.syncLines[len(m.syncLines)-1])
		}
		return pad(line, m.width)
	case m.notice != "":
		style := m.st.accent
		if m.noticeErr {
			style = m.st.errText
		}
		return fit(style.Render(render.SanitizeLine(m.notice)), m.width, 1)
	}
	return ""
}

// renderHints shows the most useful keys for what is on screen.
func (m Model) renderHints() string {
	var hints [][2]string
	switch {
	case m.inputKind == inputLinkNumber:
		hints = [][2]string{{"1-" + strconv.Itoa(len(m.links)), "link"}, {"enter", "confirm"}, {"esc", "cancel"}}
	case m.inputKind == inputConfirmDelete || m.inputKind == inputConfirmEmptyTrash:
		hints = [][2]string{{"y", "yes"}, {"any other key", "no"}}
	case m.inputKind != inputNone:
		hints = [][2]string{{"enter", "confirm"}, {"esc", "cancel"}}
	case m.focus == focusReader:
		hints = [][2]string{{"e", "archive"}, {"o", "link"}, {"J/K", "next"}, {"esc", "back"}}
	case m.focus == focusSidebar:
		hints = [][2]string{{"enter", "show"}, {"esc", "back"}}
	case m.query == queryTrash:
		hints = [][2]string{{"enter", "open"}, {"#", "restore"}, {"D", "delete now"}, {"z", "undo"}}
	default:
		hints = [][2]string{{"enter", "open"}, {"e", "archive"}, {"/", "search"}, {"z", "undo"}}
	}
	hints = append(hints, [2]string{"?", "more"})
	var parts []string
	for _, h := range hints {
		parts = append(parts, m.st.accent.Render(h[0])+" "+m.st.dim.Render(h[1]))
	}
	return fit(" "+strings.Join(parts, m.st.dim.Render("  ")), m.width, 1)
}

func (m Model) renderStatus() string {
	var parts []string
	for _, s := range m.status {
		deletion := m.st.local.Render("deletion off")
		if s.Deletion {
			deletion = m.st.warn.Render("deletion on")
		}
		synced := "never synced"
		if s.LastSynced != nil {
			synced = "synced " + ago(m.now(), *s.LastSynced)
		}
		parts = append(parts, m.st.bold.Render(render.SanitizeLine(s.Account))+": "+deletion+
			m.st.dim.Render(" · "+synced+" · ")+m.st.server.Render(fmt.Sprintf("%d on server", s.OnServer)))
	}
	if len(parts) == 0 {
		parts = append(parts, "no accounts (nou account add)")
	}
	right := "? help"
	if m.width < 30 || m.hintsShown() {
		right = ""
	}
	left := strings.Join(parts, m.st.dim.Render(" │ "))
	return fit(pad(ansi.Truncate(left, max(m.width-len(right)-2, 0), "…"), m.width-len(right))+m.st.dim.Render(right), m.width, 1)
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

// senderName shows "Name (domain)" for "Name <addr@domain>": the display
// name alone could impersonate anyone.
func senderName(from string) string {
	i := strings.Index(from, " <")
	if i <= 0 {
		return from
	}
	name := strings.Trim(from[:i], `"`)
	addr := strings.TrimSuffix(from[i+2:], ">")
	if at := strings.LastIndex(addr, "@"); at >= 0 {
		return name + " (" + addr[at+1:] + ")"
	}
	return name
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

// styledBody renders the message body with emphasis, headings and link
// references; nothing is fetched and all text is sanitized first.
func (m Model) styledBody(o *ops.ShowOut) (string, []render.Link) {
	fn := func(s lipgloss.Style) func(string) string { return func(t string) string { return s.Render(t) } }
	st := render.Styler{
		Bold:    fn(m.st.bold),
		Italic:  fn(m.st.italic),
		Heading: fn(m.st.title),
		Link:    fn(m.st.link),
		Dim:     fn(m.st.dim),
		Quote:   fn(m.st.accent),
	}
	switch o.BodyFrom {
	case "text/html":
		return render.HTMLToStyled(o.Source, st)
	case "text/plain":
		return render.TextWithLinks(strings.TrimRight(o.Source, "\n"), st)
	}
	return render.Sanitize(o.Body), nil
}
