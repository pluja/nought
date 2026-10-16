// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"errors"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/pluja/nought/internal/ops"
	"github.com/pluja/nought/internal/render"
)

// queryDrafts is the Drafts view's place in the sidebar and in goTo. It is
// not a search: NUL cannot be typed in one.
const queryDrafts = "\x00drafts"

// draftsDoneMsg brings the drafts and the outbox (to show what is waiting
// or was refused).
type draftsDoneMsg struct {
	drafts []ops.DraftInfo
	outbox []ops.OutboxEntry
	err    error
}

func (m Model) loadDrafts() tea.Cmd {
	return func() tea.Msg {
		drafts, err := ops.Call(m.ctx, m.env, permTag, ops.DraftList, struct{}{})
		outbox, outErr := ops.Call(m.ctx, m.env, permTag, ops.OutboxList, struct{}{})
		return draftsDoneMsg{drafts: drafts, outbox: outbox, err: errors.Join(err, outErr)}
	}
}

func (m Model) draftsDone(msg draftsDoneMsg) Model {
	if msg.err != nil {
		m.notice, m.noticeErr = "drafts: "+msg.err.Error(), true
	}
	m.drafts, m.outbox = msg.drafts, msg.outbox
	m.draftIdx = min(m.draftIdx, max(len(m.drafts)-1, 0))
	return m
}

// outboxEntry is the outbox entry of a draft, if it has one.
func (m Model) outboxEntry(draftID string) (ops.OutboxEntry, bool) {
	i := slices.IndexFunc(m.outbox, func(e ops.OutboxEntry) bool { return e.DraftID == draftID })
	if i < 0 {
		return ops.OutboxEntry{}, false
	}
	return m.outbox[i], true
}

func (m Model) draftsKey(key string) (Model, tea.Cmd) {
	switch key {
	case "j", "down":
		m.draftIdx = min(m.draftIdx+1, max(len(m.drafts)-1, 0))
	case "k", "up":
		m.draftIdx = max(m.draftIdx-1, 0)
	case "G", "end":
		m.draftIdx = max(len(m.drafts)-1, 0)
	case "home":
		m.draftIdx = 0
	case "D":
		if m.draftIdx < len(m.drafts) {
			m.inputKind, m.discarding = inputConfirmDiscard, m.drafts[m.draftIdx].ID
		}
	case "enter", "right":
		if m.draftIdx >= len(m.drafts) {
			return m, nil
		}
		d := m.drafts[m.draftIdx]
		// A message in the outbox comes out of it to be reviewed again.
		if e, ok := m.outboxEntry(d.ID); ok {
			return m, m.cancelSend(e.ID)
		}
		return m, m.draftCmd(ops.DraftShow, d.ID)
	}
	return m, nil
}

func (m Model) renderDrafts(w, h int) string {
	if len(m.drafts) == 0 {
		return fit(m.st.dim.Render(" No drafts."), w, h)
	}
	var lines []string
	for i, d := range m.drafts {
		to := strings.Join(d.To, ", ")
		if to == "" {
			to = "(no recipients)"
		}
		state := ""
		if e, ok := m.outboxEntry(d.ID); ok {
			state = e.State
			if e.LastError != "" {
				state += ": " + e.LastError
			}
		} else if d.LastError != "" {
			state = "not sent: " + d.LastError
		}
		gutter := " "
		if i == m.draftIdx {
			gutter = m.st.accent.Render("▌")
		}
		line := gutter + m.st.dim.Render(pad(kindTitle(d.Kind), 12)) + " " + pad(render.SanitizeLine(to), 24) + "  " +
			m.st.bold.Render(render.SanitizeLine(d.Subject))
		if state != "" {
			line += "  " + m.st.errText.Render(render.SanitizeLine(state))
		}
		line += m.st.dim.Render("  " + ago(m.now(), d.Updated))
		lines = append(lines, ansi.Truncate(line, w, "…"))
	}
	return fit(strings.Join(lines, "\n"), w, h)
}
