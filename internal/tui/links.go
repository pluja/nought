// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"errors"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/pluja/nought/internal/render"
)

// openInBrowser hands a link to the system's default handler. The URL is
// passed as a single argument, never through a shell, and only http,
// https and mailto links are accepted.
func openInBrowser(link string) error {
	if !openable(link) {
		return errors.New("only http, https and mailto links can be opened")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link) //nolint:gosec // G204: a validated http(s)/mailto URL, one argument, no shell
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link) //nolint:gosec // G204: validated URL, no shell
	default:
		cmd = exec.Command("xdg-open", link) //nolint:gosec // G204: a validated http(s)/mailto URL, one argument, no shell
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap the helper; the browser keeps running
	return nil
}

func openable(link string) bool {
	u, err := url.Parse(link)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto":
		return true
	}
	return false
}

// startLinkPick asks for a link number (o).
func (m Model) startLinkPick() (Model, tea.Cmd) {
	if m.open == nil || len(m.links) == 0 {
		m.notice = "no links in this message"
		return m, nil
	}
	return m.startInput(inputLinkNumber, "")
}

// pickLink opens the confirmation box for link n.
func (m Model) pickLink(value string) (Model, tea.Cmd) {
	n, err := strconv.Atoi(strings.Trim(value, "[] "))
	if err != nil || n < 1 || n > len(m.links) {
		m.notice, m.noticeErr = "no link "+render.SanitizeLine(value)+" (1-"+strconv.Itoa(len(m.links))+")", true
		return m, nil
	}
	m.linkPick = n
	return m, nil
}

// linkKey handles the confirmation box: open, copy, or cancel.
func (m Model) linkKey(key string) (Model, tea.Cmd) {
	link := m.links[m.linkPick-1]
	clean := render.CleanURL(link.URL).URL
	m.linkPick = 0
	switch key {
	case "o", "y", "enter":
		return m.openLink(link.URL, "opened ")
	case "O":
		return m.openLink(clean, "opened without tracking: ")
	case "c":
		m.notice = "copied link to the clipboard"
		return m, tea.SetClipboard(link.URL)
	case "C":
		m.notice = "copied the link without tracking"
		return m, tea.SetClipboard(clean)
	}
	m.notice = "link not opened"
	return m, nil
}

func (m Model) openLink(link, what string) (Model, tea.Cmd) {
	if err := m.openURL(link); err != nil {
		m.notice, m.noticeErr = "could not open the link: "+err.Error(), true
		return m, nil
	}
	m.notice = what + render.Link{URL: link}.Host()
	return m, nil
}

// renderLinkBox is the confirmation shown before a link is opened.
func (m Model) renderLinkBox() string {
	link := m.links[m.linkPick-1]
	w := max(min(m.width-4, 76), 20)
	inner := w - 4
	host := link.Host()
	if host == "" {
		host = strings.SplitN(link.URL, ":", 2)[0]
	}
	lines := []string{
		m.st.title.Render("Open link [" + strconv.Itoa(m.linkPick) + "]?"),
		"",
	}
	if link.Label != "" {
		lines = append(lines, m.st.dim.Render("Text:    ")+ansi.Truncate(render.SanitizeLine(link.Label), inner-9, "…"))
	}
	lines = append(lines,
		m.st.dim.Render("Goes to: ")+m.st.bold.Render(render.SanitizeLine(host)),
		m.st.dim.Render(ansi.Truncate(render.SanitizeLine(link.URL), inner, "…")),
		"")
	if link.Misleading() {
		lines = append(lines, m.st.errText.Render("Warning: the text names a different site than the link goes to."), "")
	}
	clean := render.CleanURL(link.URL)
	switch {
	case clean.Changed(link.URL):
		what := "Without tracking:"
		if clean.Unwrapped != "" {
			what = "Without tracking (skips the redirect via " + render.SanitizeLine(clean.Unwrapped) + "):"
		}
		lines = append(lines, m.st.local.Render(ansi.Truncate(what, inner, "…")),
			m.st.dim.Render(ansi.Truncate(render.SanitizeLine(clean.URL), inner, "…")))
		if len(clean.Removed) > 0 {
			lines = append(lines, m.st.dim.Render(ansi.Truncate("removed: "+render.SanitizeLine(strings.Join(clean.Removed, ", ")), inner, "…")))
		}
		lines = append(lines, "")
	case clean.Opaque:
		lines = append(lines, m.st.warn.Render("This goes through a tracking redirect; the real destination"),
			m.st.warn.Render("is hidden, so it cannot be cleaned."), "")
	}
	lines = append(lines,
		m.st.dim.Render("Opening it tells the sender you clicked (links often track you)."),
		"",
		m.st.accent.Render("[o]")+" open   "+m.st.accent.Render("[O]")+" open clean   "+
			m.st.accent.Render("[c]")+" copy   "+m.st.accent.Render("[C]")+" copy clean   "+m.st.accent.Render("[esc]"))
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("6")).
		Padding(0, 1).Width(w).Render(strings.Join(lines, "\n"))
	return box
}

// overlay draws box centered over screen.
func overlay(screen, box string, width, height int) string {
	lines := strings.Split(screen, "\n")
	boxLines := strings.Split(box, "\n")
	top := max((height-len(boxLines))/2, 0)
	for i, bl := range boxLines {
		if top+i >= len(lines) {
			break
		}
		left := max((width-ansi.StringWidth(bl))/2, 0)
		lines[top+i] = pad(strings.Repeat(" ", left)+bl, width)
	}
	return strings.Join(lines, "\n")
}

// linksFooter and linksList are shown at the end of the reader.
func (m Model) linksFooter(w int) string {
	if len(m.links) == 0 {
		return ""
	}
	if !m.showLinks {
		n := strconv.Itoa(len(m.links)) + " link"
		if len(m.links) > 1 {
			n += "s"
		}
		return "\n\n" + m.st.dim.Render(n+" · o open · L show")
	}
	var b strings.Builder
	b.WriteString("\n\n" + m.st.title.Render("Links") + m.st.dim.Render("  (o open · L hide)"))
	for i, l := range m.links {
		host := l.Host()
		if host == "" {
			host = l.URL
		}
		num := m.st.dim.Render("[" + strconv.Itoa(i+1) + "] ")
		line := num + m.st.bold.Render(render.SanitizeLine(host)) + " " + m.st.dim.Render(render.SanitizeLine(l.URL))
		if l.Misleading() {
			line = num + m.st.errText.Render("! ") + m.st.bold.Render(render.SanitizeLine(host)) + " " + m.st.dim.Render(render.SanitizeLine(l.URL))
		}
		b.WriteString("\n" + ansi.Truncate(line, w, "…"))
	}
	return b.String()
}
