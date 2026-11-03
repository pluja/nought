// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"cmp"
	"maps"
	"slices"
	"strconv"

	tea "charm.land/bubbletea/v2"

	"github.com/pluja/nought/internal/ops"
)

// Batch actions: x marks the selected message, V marks the messages the
// cursor passes over, * marks the whole view. While any are marked, the
// message actions (e u s # t D) apply to all of them at once; replies and
// saving attachments still take one message. Marks belong to a view, and
// last until Esc or until their messages leave it.

// markAllMsg brings the whole view's messages for *.
type markAllMsg struct {
	gen  int
	msgs []ops.MessageOut
	more bool // the view holds more than ops.MaxBatch
	err  error
}

// actionTargets is what a message action applies to: in the reader, the
// message read; elsewhere, the marked messages, or the selected one. Marked messages that are shown come as
// listed now; others (marked by * but not loaded) as they were when
// marked, which a sync in between may have changed: undo then restores
// those tags as they were seen.
func (m Model) actionTargets() ([]ops.MessageOut, bool) {
	if len(m.marked) == 0 || m.focus == focusReader {
		msg, ok := m.target()
		if !ok {
			return nil, false
		}
		return []ops.MessageOut{msg}, true
	}
	out := make([]ops.MessageOut, 0, len(m.marked))
	shown := map[msgKey]bool{}
	for _, msg := range m.msgs {
		if _, ok := m.marked[keyOf(msg)]; ok {
			out = append(out, msg)
			shown[keyOf(msg)] = true
		}
	}
	var rest []ops.MessageOut
	for k, msg := range m.marked {
		if !shown[k] {
			rest = append(rest, msg)
		}
	}
	slices.SortFunc(rest, func(a, b ops.MessageOut) int {
		return cmp.Or(cmp.Compare(a.Account, b.Account), cmp.Compare(a.ID, b.ID))
	})
	return append(out, rest...), true
}

// mark adds messages to the marks, up to what one action takes. The map
// is copied first: models are values, and an older copy must not see the
// change.
func (m Model) mark(msgs ...ops.MessageOut) Model {
	marked := maps.Clone(m.marked)
	if marked == nil {
		marked = map[msgKey]ops.MessageOut{}
	}
	for _, msg := range msgs {
		if _, ok := marked[keyOf(msg)]; !ok && len(marked) >= ops.MaxBatch {
			m.notice = "at most " + strconv.Itoa(ops.MaxBatch) + " marked at once"
			break
		}
		marked[keyOf(msg)] = msg
	}
	m.marked = marked
	return m
}

// pruneMarks drops the marks of messages that left the view (a sync,
// another device), when the view is all loaded and so would list them.
// With more to load, a mark from * may be on a message not loaded yet.
func (m Model) pruneMarks() Model {
	if m.more || len(m.marked) == 0 {
		return m
	}
	listed := map[msgKey]bool{}
	for _, msg := range m.msgs {
		listed[keyOf(msg)] = true
	}
	m.marked = maps.Clone(m.marked)
	maps.DeleteFunc(m.marked, func(k msgKey, _ ops.MessageOut) bool { return !listed[k] })
	return m
}

func (m Model) clearMarks() Model {
	m.marked, m.ranging = nil, false
	return m
}

// toggleMark marks or unmarks the selected message, then moves on. It
// ends a range, which would mark the message again.
func (m Model) toggleMark() (Model, tea.Cmd) {
	sel, ok := m.selected()
	if !ok {
		return m, nil
	}
	m.ranging = false
	if _, marked := m.marked[keyOf(sel)]; marked {
		m.marked = maps.Clone(m.marked)
		delete(m.marked, keyOf(sel))
	} else {
		m = m.mark(sel)
	}
	return m.move(1)
}

// toggleRange starts marking from the cursor as it moves, or stops.
func (m Model) toggleRange() (Model, tea.Cmd) {
	if m.ranging {
		m.ranging = false
		return m, nil
	}
	sel, ok := m.selected()
	if !ok {
		return m, nil
	}
	m.ranging, m.rangeAnchor = true, keyOf(sel)
	return m.mark(sel), nil
}

// markRange marks every message between the range's anchor and the
// cursor. The anchor is a message, not a row: mail arriving at the top
// must not move the range. If the anchor left the view, the range ends.
// Moving back does not unmark: x does.
func (m Model) markRange() Model {
	if !m.ranging || len(m.msgs) == 0 {
		return m
	}
	from := slices.IndexFunc(m.msgs, func(msg ops.MessageOut) bool { return keyOf(msg) == m.rangeAnchor })
	if from < 0 {
		m.ranging = false
		return m
	}
	lo, hi := min(from, m.cursor), max(from, m.cursor)
	return m.mark(m.msgs[lo : hi+1]...)
}

// markAll marks every message of the view, also those not loaded yet.
func (m Model) markAll() (Model, tea.Cmd) {
	if m.query == queryDrafts {
		return m, nil
	}
	m.markGen++
	m.markingAll, m.notice = true, "marking…"
	gen, q := m.markGen, m.query
	return m, func() tea.Msg {
		out, err := ops.Call(m.ctx, m.env, permTag, ops.MsgSearch, ops.SearchIn{Query: q, Limit: ops.MaxBatch})
		return markAllMsg{gen: gen, msgs: out.Messages, more: out.More, err: err}
	}
}

func (m Model) markAllDone(msg markAllMsg) (Model, tea.Cmd) {
	if msg.gen != m.markGen {
		return m, nil // the view changed meanwhile
	}
	m.markingAll = false
	if msg.err != nil {
		m.notice, m.noticeErr = msg.err.Error(), true
		return m, nil
	}
	m.notice = ""
	m = m.mark(msg.msgs...)
	switch {
	case m.notice != "": // over the most at once, with earlier marks
	case msg.more:
		m.notice = "the first " + strconv.Itoa(len(m.marked)) + " marked (the most at once)"
	default:
		m.notice = "all " + strconv.Itoa(len(m.marked)) + " marked"
	}
	return m, nil
}

// escMarks ends a range, then clears the marks.
func (m Model) escMarks() Model {
	switch {
	case m.ranging:
		m.ranging = false
	case len(m.marked) > 0:
		m = m.clearMarks()
		m.notice = "marks cleared"
	}
	return m
}

// hasTag is a predicate on a message's tags.
func hasTag(tag string) func(ops.MessageOut) bool {
	return func(msg ops.MessageOut) bool { return slices.Contains(msg.Tags, tag) }
}

// all reports whether every message satisfies ok.
func all(msgs []ops.MessageOut, ok func(ops.MessageOut) bool) bool {
	return !slices.ContainsFunc(msgs, func(msg ops.MessageOut) bool { return !ok(msg) })
}

// enqueueAction runs an action's tag change. The marks stay for the next
// action, with their copies changed too, except on messages that leave the
// view.
func (m Model) enqueueAction(req tagReq) (Model, tea.Cmd) {
	gone := m.leavesView(req)
	m.ranging = false
	m.marked = maps.Clone(m.marked)
	for _, msg := range req.msgs {
		k := keyOf(msg)
		marked, ok := m.marked[k]
		switch {
		case !ok:
		case gone:
			delete(m.marked, k)
		default:
			marked.Tags = applyTagChange(marked.Tags, req.add, req.remove)
			m.marked[k] = marked
		}
	}
	return m.enqueue(req)
}

func (m Model) unmark(msgs []ops.MessageOut) Model {
	m.marked = maps.Clone(m.marked)
	for _, msg := range msgs {
		delete(m.marked, keyOf(msg))
	}
	return m
}
