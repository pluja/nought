// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

// Writing mail. r, R, f and c start a draft in the user's editor; when it
// closes, the review screen shows the message and its checks. y sends it
// after the undo window (z takes it back); quitting ends the window.

import (
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/pluja/nought/internal/compose"
	"github.com/pluja/nought/internal/ops"
	"github.com/pluja/nought/internal/render"
)

// Permissions for writing and sending: the TUI acts for the local user.
const (
	permDraft = ops.Draft
	permSend  = ops.Send
)

const (
	// retryDelay is how soon a message the server could not take now is
	// tried again (syncs try too).
	retryDelay = 2 * time.Minute
	// flushMargin lets the undo window close before the timer sends.
	flushMargin = 200 * time.Millisecond
	// busyRetry is how soon to try again while another nou is sending.
	busyRetry = 5 * time.Second
	// maxSuggestions is how many addresses the To input proposes.
	maxSuggestions = 5
)

type (
	// draftMsg brings a draft: to open in the editor (edit), or to review.
	draftMsg struct {
		out  ops.DraftOut
		edit bool
		err  error
	}
	editorDoneMsg struct{ err error }
	queuedMsg     struct {
		entry ops.OutboxEntry
		err   error
	}
	// cancelDoneMsg brings a message back from the outbox to review.
	cancelDoneMsg struct {
		entryID string
		out     ops.DraftOut
		err     error
	}
	flushMsg     struct{}
	flushDoneMsg struct {
		out  ops.FlushOut
		quit bool // the flush that ends the session
		err  error
	}
	discardDoneMsg struct{ err error }
	suggestDoneMsg struct {
		token string
		list  []string
	}
)

// execEditor runs the editor on a file, the TUI suspended meanwhile.
func execEditor(command []string) func(string) tea.Cmd {
	if len(command) == 0 {
		command = []string{"vi"}
	}
	return func(path string) tea.Cmd {
		c := exec.Command(command[0], append(slices.Clone(command[1:]), path)...) //nolint:gosec // G204: the user's own editor
		return tea.ExecProcess(c, func(err error) tea.Msg { return editorDoneMsg{err: err} })
	}
}

// startDraft creates a draft for r (reply), R (reply all), f (forward) or c
// (new), then opens it in the editor.
func (m Model) startDraft(key string) (Model, tea.Cmd) {
	sel, ok := m.selected()
	if key != "c" && !ok {
		return m, nil
	}
	ctx, env := m.ctx, m.env
	return m, func() tea.Msg {
		var out ops.DraftOut
		var err error
		switch key {
		case "r", "R":
			out, err = ops.Call(ctx, env, permDraft, ops.DraftReply, ops.DraftReplyIn{ID: sel.ID, Account: sel.Account, All: key == "R"})
		case "f":
			out, err = ops.Call(ctx, env, permDraft, ops.DraftForward, ops.DraftForwardIn{ID: sel.ID, Account: sel.Account})
		default: // from the selected message's account, if any
			out, err = ops.Call(ctx, env, permDraft, ops.DraftNew, ops.DraftNewIn{Account: sel.Account})
		}
		return draftMsg{out: out, edit: true, err: err}
	}
}

func (m Model) draftReady(msg draftMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		m.notice, m.noticeErr = msg.err.Error(), true
		return m, nil
	}
	if msg.edit {
		m.editing = msg.out.ID
		return m, m.editFile(msg.out.Path)
	}
	if msg.out.Discarded {
		m.composing, m.notice = nil, "nothing written; discarded"
		return m.refresh()
	}
	m.composing = &msg.out
	return m, nil
}

// editorDone reads the draft again; the draft was saved by the editor,
// so nothing is lost if the editor failed.
func (m Model) editorDone(msg editorDoneMsg) (Model, tea.Cmd) {
	id := m.editing
	m.editing = ""
	if msg.err != nil {
		m.notice, m.noticeErr = "the editor failed ("+msg.err.Error()+"); the draft is kept in Drafts (gd)", true
		return m, m.loadSidebar()
	}
	return m, m.draftCmd(ops.DraftUpdate, id)
}

func (m Model) draftCmd(op ops.Op[ops.DraftIDIn, ops.DraftOut], id string) tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, permDraft, op, ops.DraftIDIn{ID: id})
		return draftMsg{out: out, err: err}
	}
}

func (m Model) reviewKey(key string) (Model, tea.Cmd) {
	d := m.composing
	switch key {
	case "y":
		if i := slices.IndexFunc(d.Problems, func(p compose.Problem) bool { return p.Blocking }); i >= 0 {
			m.notice, m.noticeErr = "cannot send: "+d.Problems[i].Text, true
			return m, nil
		}
		return m, func() tea.Msg {
			e, err := ops.Call(m.ctx, m.env, permSend, ops.MsgSend, ops.SendIn{DraftID: d.ID})
			return queuedMsg{entry: e, err: err}
		}
	case "e":
		m.editing = d.ID
		return m, m.editFile(d.Path)
	case "t":
		m.suggestions = nil
		return m.startInput(inputTo, strings.Join(d.Draft.To, ", "))
	case "a":
		return m.startInput(inputAttach, "")
	case "d", "esc":
		m.composing = nil
		m.notice = "kept in Drafts (gd)"
		return m.refresh()
	case "q":
		m.inputKind = inputConfirmDiscard
	case "?":
		m.help = true
	}
	return m, nil
}

// queued starts the undo window of a message just sent.
func (m Model) queued(msg queuedMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		m.notice, m.noticeErr = msg.err.Error(), true
		return m, nil
	}
	m.composing = nil
	m.sendingID, m.sendingSubject, m.sendingAfter = msg.entry.ID, msg.entry.Subject, msg.entry.SendAfter
	wait := max(msg.entry.SendAfter.Sub(m.now()), 0)
	m.notice = "sending…"
	if wait > 0 {
		m.notice = "sending in " + shortDuration(wait) + "… z undoes"
	}
	next, cmd := m.refresh()
	return next, tea.Batch(cmd, m.after(wait+flushMargin, func(time.Time) tea.Msg { return flushMsg{} }))
}

// cancelSend takes a message out of the outbox, back to review.
func (m Model) cancelSend(id string) tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, permDraft, ops.OutboxCancel, ops.DraftIDIn{ID: id})
		return cancelDoneMsg{entryID: id, out: out, err: err}
	}
}

func (m Model) cancelDone(msg cancelDoneMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		if errors.Is(msg.err, ops.ErrNotFound) {
			msg.err = errors.New("too late: it was sent")
		}
		m.notice, m.noticeErr = msg.err.Error(), true
		return m, nil
	}
	if msg.entryID == m.sendingID {
		m.sendingID = ""
	}
	m.composing = &msg.out
	m.notice = "not sent; back to review"
	return m, nil
}

// flush sends what is due; all also sends what is in its undo window.
func (m Model) flush(all bool) tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, permSend, ops.OutboxFlush, ops.FlushIn{All: all})
		return flushDoneMsg{out: out, quit: all, err: err}
	}
}

func (m Model) flushDone(msg flushDoneMsg) (Model, tea.Cmd) {
	var again tea.Cmd
	failed := msg.err != nil
	if m.sendingID != "" {
		var notice string
		notice, failed, again = m.sendingOutcome(msg)
		if notice != "" {
			m.notice = notice
		}
	}
	m.notice = joinNotice(m.notice, msg.out.Notes...)
	if msg.err != nil {
		m.notice = joinNotice(m.notice, "send: "+msg.err.Error())
	}
	m.noticeErr = failed
	if msg.quit {
		m.quitSending = false
		if !failed && m.sendingID == "" {
			return m.requestQuit()
		}
		// Not sent: say so instead of leaving. The message stays in the
		// outbox and goes at a later sync.
		m.sendingID = ""
		m.notice = joinNotice(m.notice, "q quits")
		return m, nil
	}
	next, cmd := m.refresh()
	return next, tea.Batch(again, cmd)
}

// sendingOutcome reads what became of the message in its undo window, and
// clears sendingID once it needs no more watching.
func (m *Model) sendingOutcome(msg flushDoneMsg) (notice string, failed bool, again tea.Cmd) {
	flushAgain := func(d time.Duration) tea.Cmd {
		return m.after(d, func(time.Time) tea.Msg { return flushMsg{} })
	}
	i := slices.IndexFunc(msg.out.Results, func(e ops.OutboxEntry) bool { return e.ID == m.sendingID })
	if i < 0 { // not tried: its window is still open, or another nou is sending
		wait := max(m.sendingAfter.Sub(m.now()), busyRetry)
		return "", msg.err != nil, flushAgain(wait + flushMargin)
	}
	e := msg.out.Results[i]
	m.sendingID = ""
	switch e.State {
	case ops.StateSent:
		return "sent: " + e.Subject, msg.err != nil, nil
	case ops.StateFailed:
		return "not sent: " + e.LastError + " (it waits in Drafts, gd)", true, nil
	case ops.StateSending:
		return "may have been sent: " + e.LastError + " (see Drafts, gd)", true, nil
	}
	return "not sent yet (" + e.LastError + "); trying again in " + shortDuration(retryDelay), true, flushAgain(retryDelay)
}

// joinNotice adds remarks to a notice.
func joinNotice(notice string, more ...string) string {
	return strings.Join(slices.DeleteFunc(append([]string{notice}, more...), func(s string) bool { return s == "" }), "; ")
}

// suggest proposes known addresses for the word being typed in To.
func (m Model) suggest(account, token string) tea.Cmd {
	return func() tea.Msg {
		found, err := ops.Call(m.ctx, m.env, permTag, ops.AddressSuggest, ops.SuggestIn{Account: account, Prefix: token})
		if err != nil {
			return suggestDoneMsg{token: token}
		}
		var list []string
		for _, a := range found[:min(len(found), maxSuggestions)] {
			list = append(list, a.Address)
		}
		return suggestDoneMsg{token: token, list: list}
	}
}

// lastAddress is the address being typed: after the last comma.
func lastAddress(s string) string {
	return strings.TrimSpace(s[strings.LastIndex(s, ",")+1:])
}

// complete replaces the address being typed with the first suggestion.
func (m Model) complete() Model {
	if len(m.suggestions) == 0 {
		return m
	}
	v, prefix := m.input.Value(), ""
	if i := strings.LastIndex(v, ","); i >= 0 {
		prefix = v[:i+1] + " "
	}
	m.input.SetValue(prefix + m.suggestions[0] + ", ")
	m.input.CursorEnd()
	m.suggestions = nil
	return m
}

func (m Model) setDraft(in ops.DraftSetIn) tea.Cmd {
	return func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, permDraft, ops.DraftSet, in)
		return draftMsg{out: out, err: err}
	}
}

func (m Model) discard(id string) tea.Cmd {
	return func() tea.Msg {
		_, err := ops.Call(m.ctx, m.env, permDraft, ops.DraftDiscard, ops.DraftIDIn{ID: id})
		return discardDoneMsg{err: err}
	}
}

// shortDuration is "10s" or "2m 30s".
func shortDuration(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s%60 == 0 {
		return fmt.Sprintf("%dm", s/60)
	}
	return fmt.Sprintf("%dm %ds", s/60, s%60)
}

// kindTitle names what is being written.
func kindTitle(kind string) string {
	switch kind {
	case ops.KindReply:
		return "Reply"
	case ops.KindReplyAll:
		return "Reply to all"
	case ops.KindForward:
		return "Forward"
	}
	return "New message"
}

// renderReview shows the message as it will be sent, its checks and the
// keys, in place of the list and reader.
func (m Model) renderReview() string {
	d, w, h := m.composing, m.width, m.bodyHeight()
	inner := max(w-2, 1)
	wrap := lipgloss.NewStyle().Width(inner)
	var head []string
	field := func(name, value string) {
		head = append(head, wrap.Render(m.st.dim.Render(fmt.Sprintf("%-8s", name+":"))+" "+render.SanitizeLine(value)))
	}
	field("From", d.Draft.From)
	field("To", strings.Join(d.Draft.To, ", "))
	if len(d.Draft.Cc) > 0 {
		field("Cc", strings.Join(d.Draft.Cc, ", "))
	}
	if len(d.Draft.Bcc) > 0 {
		field("Bcc", strings.Join(d.Draft.Bcc, ", "))
	}
	head = append(head, wrap.Render(m.st.dim.Render("Subject:")+" "+m.st.bold.Render(render.SanitizeLine(d.Draft.Subject))))
	if len(d.Draft.Attach) > 0 {
		field("Attach", strings.Join(d.Draft.Attach, ", "))
	}
	head = append(head, m.st.dim.Render(strings.Repeat("─", inner)))

	var tail []string
	for _, p := range d.Problems {
		if p.Blocking {
			tail = append(tail, m.st.errText.Render(m.glyph("✗ ", "x ")+render.SanitizeLine(p.Text)))
		} else {
			tail = append(tail, m.st.warn.Render(m.glyph("⚠ ", "! ")+render.SanitizeLine(p.Text)))
		}
	}
	keys := [][2]string{{"y", "send"}, {"e", "edit"}, {"t", "to"}, {"a", "attach"}, {"d", "keep as draft"}, {"q", "discard"}}
	var parts []string
	for _, k := range keys {
		parts = append(parts, m.st.accent.Render(k[0])+" "+m.st.dim.Render(k[1]))
	}
	tail = append(tail, "", strings.Join(parts, m.st.dim.Render("  ")))

	room := max(h-len(head)-len(tail), 1)
	body := strings.Split(wrap.Render(render.Sanitize(strings.TrimRight(d.Draft.Body, "\n"))), "\n")
	if len(body) > room {
		body = append(body[:room-1], m.st.dim.Render("… (e opens the whole message)"))
	}
	for i, line := range body {
		if strings.HasPrefix(line, ">") {
			body[i] = m.st.dim.Render(line)
		}
	}
	lines := slices.Concat(head, body, make([]string, max(room-len(body), 0)), tail)
	for i := range lines {
		lines[i] = " " + lines[i]
	}
	return fit(strings.Join(lines, "\n"), w, h)
}
