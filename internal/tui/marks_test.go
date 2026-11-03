// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/pluja/nought/internal/ops"
)

// subjects lists, newest first, the subjects of the messages a query finds.
func subjects(t *testing.T, m Model, q string) []string {
	t.Helper()
	out, err := ops.Call(t.Context(), m.env, ops.Read, ops.MsgSearch, ops.SearchIn{Query: q})
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, msg := range out.Messages {
		s = append(s, msg.Subject)
	}
	return s
}

// x marks the selected message and moves on; actions then apply to every
// marked message at once, and one z undoes them all.
func TestMarkArchiveUndo(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, keys("xx")...)
	if s := screen(m); !strings.Contains(s, "2 marked") || strings.Count(s, "✓") != 2 {
		t.Fatalf("two marks not shown:\n%s", s)
	}
	m = drive(t, m, key("e"))
	if got := subjects(t, m, "tag:inbox"); len(got) != 1 {
		t.Fatalf("inbox after archiving two = %v", got)
	}
	if !strings.Contains(m.notice, "archived: 2 messages") || len(m.msgs) != 1 {
		t.Errorf("notice %q, %d rows", m.notice, len(m.msgs))
	}
	if strings.Contains(screen(m), "marked") {
		t.Error("marks kept after the action")
	}
	if sel, _ := m.selected(); sel.Subject != m.msgs[0].Subject || !strings.Contains(sel.Subject, "evil") {
		t.Errorf("selection moved to %q, want the message it was on", sel.Subject)
	}
	m = drive(t, m, key("z"))
	if got := subjects(t, m, "tag:inbox"); len(got) != 3 {
		t.Errorf("inbox after undo = %v", got)
	}
}

// A batch undo restores each message as it was: here only the two that
// were unread become unread again.
func TestBatchUndoIsExact(t *testing.T) {
	m := newTestModel(t, 120, 30)
	if _, err := ops.Call(t.Context(), m.env, ops.Tag, ops.MsgTag, ops.TagIn{Query: "subject:invoice", Remove: []string{"unread"}}); err != nil {
		t.Fatal(err)
	}
	m = drive(t, m, key("S")) // reload what changed underneath
	m = drive(t, m, key("*"))
	if !strings.Contains(m.notice, "all 3 marked") {
		t.Fatalf("notice %q", m.notice)
	}
	m = drive(t, m, key("u"))
	if got := subjects(t, m, "is:unread"); len(got) != 0 {
		t.Fatalf("unread after marking all read = %v", got)
	}
	m = drive(t, m, key("z"))
	got := subjects(t, m, "is:unread")
	if len(got) != 2 || slices.Contains(got, "Invoice 2026-114") {
		t.Errorf("unread after undo = %v, want the two that were", got)
	}
}

// Marks outlast an action, so marked mail can be read and then archived;
// those of messages that leave the view (archived from the inbox) go.
func TestMarksStayForTheNextAction(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, keys("xxu")...)
	if !strings.Contains(screen(m), "2 marked") {
		t.Fatalf("marks gone after u:\n%s", screen(m))
	}
	m = drive(t, m, key("e"))
	if got := subjects(t, m, "-tag:inbox -is:unread"); len(got) != 2 {
		t.Errorf("read and archived = %v, want the two marked", got)
	}
	if len(m.marked) != 0 {
		t.Errorf("%d still marked after they left the inbox", len(m.marked))
	}
}

// In a search the archived mail stays listed, and so marked: a filtered
// view can be marked once and acted on many times. Trashing takes it out.
func TestMarksStayInASearch(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, keys("ga")...)
	m = drive(t, m, key("*"))
	m = drive(t, m, key("e"))
	if got := subjects(t, m, "tag:inbox"); len(got) != 0 || len(m.marked) != 3 {
		t.Fatalf("inbox %v, %d marked after archiving all in a search", got, len(m.marked))
	}
	m = drive(t, m, key("#"))
	if got := subjects(t, m, "tag:deleted"); len(got) != 3 || len(m.marked) != 0 {
		t.Errorf("trash %v, %d marked after trashing them", got, len(m.marked))
	}
}

// A mark on a message not loaded (by *) keeps a copy of it; an action
// updates the copy, so the next u toggles back and undo stays exact.
func TestMarkCopiesFollowActions(t *testing.T) {
	m := newTestModel(t, 120, 30)
	unloaded := ops.MessageOut{ID: "0123456789ab", Account: "personal", Tags: []string{"inbox", "unread"}}
	m, _ = m.markAllDone(markAllMsg{gen: m.markGen, msgs: []ops.MessageOut{unloaded}})
	m, _ = m.messageAction("u")
	if got, ok := m.marked[keyOf(unloaded)]; !ok || slices.Contains(got.Tags, ops.TagUnread) {
		t.Errorf("copy of the unloaded mark after u: %v (marked %v)", got.Tags, ok)
	}
}

// V marks everything the cursor passes over; Esc ends the range, a second
// Esc clears the marks.
func TestRangeMarks(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("V"), key("j"), key("j"))
	if !strings.Contains(screen(m), "3 marked") {
		t.Fatalf("range:\n%s", screen(m))
	}
	m = drive(t, m, key("esc"), key("k"))
	if !strings.Contains(screen(m), "3 marked") {
		t.Error("moving after the range ended changed the marks")
	}
	m = drive(t, m, key("esc"))
	if strings.Contains(screen(m), "marked") {
		t.Error("Esc did not clear the marks")
	}
}

// Deleting marked mail for good asks once, with the count.
func TestDeleteMarkedAsks(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, keys("xxD")...)
	if s := screen(m); !strings.Contains(s, "Delete permanently") || !strings.Contains(s, "2 messages") {
		t.Fatalf("prompt:\n%s", s)
	}
	m = drive(t, m, key("y"))
	if got := subjects(t, m, "* OR tag:deleted"); len(got) != 1 || !strings.Contains(got[0], "evil") {
		t.Errorf("left after deleting the two marked for good = %v", got)
	}
}

// The tag prompt applies to every marked message.
func TestTagMarked(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, keys("xxt")...)
	m = drive(t, m, append(keys("+work"), key("enter"))...)
	if got := subjects(t, m, "tag:work"); len(got) != 2 {
		t.Errorf("tagged %v", got)
	}
}

// Replies answer one message: with marks, r says so and drafts nothing.
func TestReplyWithMarksRefuses(t *testing.T) {
	m, _ := newSendModel(t)
	opened := 0
	m.editFile = func(string) tea.Cmd { opened++; return nil }
	m = drive(t, m, keys("xr")...)
	if opened != 0 || !strings.Contains(m.notice, "one message") {
		t.Errorf("editor opened %d times, notice %q", opened, m.notice)
	}
}

// Marks belong to a view: going elsewhere clears them.
func TestMarksClearWithTheView(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("x"))
	m = drive(t, m, keys("gu")...)
	if strings.Contains(screen(m), "marked") {
		t.Error("marks followed into another view")
	}
}

// Review: :tag acted on the message a tag prompt was last opened on, not
// the one selected.
func TestPaletteTagActsOnTheSelection(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("t"), key("esc"), key("j"))
	m = drive(t, m, append(keys(":tag +work"), key("enter"))...)
	if got := subjects(t, m, "tag:work"); !slices.Equal(got, []string{m.msgs[1].Subject}) {
		t.Errorf("tagged %v, want the selected %q", got, m.msgs[1].Subject)
	}
}

// A * still loading when the view changes marks nothing in the new one.
func TestMarkAllForAnOldView(t *testing.T) {
	m := newTestModel(t, 120, 30)
	next, cmd := m.Update(key("*"))
	m = drive(t, next.(Model), keys("gu")...)
	m = drive(t, m, run(cmd)...)
	if len(m.marked) != 0 {
		t.Errorf("%d marked in the new view", len(m.marked))
	}
}

// # restores when every message is in the trash (taking off each one's
// expiry), and otherwise trashes only those that are not.
func TestTrashReqOnASelection(t *testing.T) {
	m := newTestModel(t, 120, 30)
	trashed := ops.MessageOut{ID: "a", Account: "p", Tags: []string{"deleted", "delete-after:2026-11-01"}}
	trashed2 := ops.MessageOut{ID: "b", Account: "p", Tags: []string{"deleted", "delete-after:2026-11-09"}}
	plain := ops.MessageOut{ID: "c", Account: "p", Tags: []string{"inbox"}}

	req, err := m.trashReq([]ops.MessageOut{trashed, plain})
	if err != nil || len(req.msgs) != 1 || req.msgs[0].ID != "c" || !slices.Contains(req.add, "deleted") {
		t.Errorf("mixed: %+v, %v; want only the one not in the trash, trashed", req, err)
	}
	req, err = m.trashReq([]ops.MessageOut{trashed, trashed2})
	want := []string{"deleted", "delete-after:2026-11-01", "delete-after:2026-11-09"}
	if err != nil || len(req.msgs) != 2 || !slices.Equal(req.remove, want) || !slices.Equal(req.add, []string{"inbox"}) {
		t.Errorf("all trashed: add %v remove %v, %v; want a restore of both", req.add, req.remove, err)
	}
}

// * reports when a view holds more than one action can take.
func TestMarkAllCapped(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m, _ = m.markAllDone(markAllMsg{gen: m.markGen, msgs: m.msgs, more: true})
	if !strings.Contains(m.notice, "the first 3 marked") {
		t.Errorf("notice %q", m.notice)
	}
}

// Review: V remembered its start as a row, so mail arriving at the top
// moved the range onto other messages. It is anchored to the message.
func TestRangeSurvivesARefresh(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("j"), key("V")) // anchored on the second message
	anchor := m.msgs[1]
	newer := ops.MessageOut{ID: "000000000000", Account: "personal", Subject: "new", Tags: []string{"inbox"}}
	m = drive(t, m, searchDoneMsg{gen: m.searchGen, out: ops.SearchOut{Messages: append([]ops.MessageOut{newer}, m.msgs...)}})
	m = drive(t, m, key("j"))
	got, _ := m.actionTargets()
	var subjects []string
	for _, msg := range got {
		subjects = append(subjects, msg.Subject)
	}
	if len(got) != 2 || got[0].Subject != anchor.Subject || slices.Contains(subjects, "new") {
		t.Errorf("range covers %v, want %q and the one after it", subjects, anchor.Subject)
	}
}

// Review: x during V was undone by the range at once; x ends the range.
func TestUnmarkDuringRange(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("V"), key("j"), key("j"), key("k"), key("x"))
	if _, marked := m.marked[keyOf(m.msgs[1])]; marked || len(m.marked) != 2 {
		t.Errorf("%d marked, the unmarked one marked %v", len(m.marked), marked)
	}
}

// Review: with marks, the reader acted on them, not on the message read.
func TestReaderActsOnTheMessageRead(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("x"))     // mark the first
	m = drive(t, m, key("enter")) // read the second
	read := m.open.Message.Subject
	m = drive(t, m, key("e"))
	if got := subjects(t, m, "tag:inbox"); len(got) != 2 || slices.Contains(got, read) {
		t.Errorf("inbox %v: want only %q archived", got, read)
	}
}

// Review: marks of messages that left the view (a sync, another device)
// were still acted on, unseen. With the whole view loaded, they go.
func TestMarksLeaveWithTheirMessages(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, keys("xx")...)
	m = drive(t, m, searchDoneMsg{gen: m.searchGen, out: ops.SearchOut{Messages: m.msgs[1:]}})
	if len(m.marked) != 1 {
		t.Errorf("%d marked after one marked message left the view", len(m.marked))
	}
}

// Review: an action right after * ran before the marks arrived, on the
// selected message alone. It waits for them.
func TestActionWaitsForMarkAll(t *testing.T) {
	m := newTestModel(t, 120, 30)
	next, cmd := m.Update(key("*"))
	m = drive(t, next.(Model), key("e"))
	if got := subjects(t, m, "tag:inbox"); len(got) != 3 || !strings.Contains(m.notice, "marking") {
		t.Errorf("inbox %v, notice %q: acted before the marks arrived", got, m.notice)
	}
	m = drive(t, m, run(cmd)...)
	if len(m.marked) != 3 {
		t.Errorf("%d marked", len(m.marked))
	}
}

// Marked rows stand out in a hue of their own (not just a tick), and the
// cursor stays visible on them.
func TestMarkedRowsAreTinted(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("x")) // marks the first, cursor on the second
	const markBG, cursorBG = ";44m", ";100m"
	marked := m.renderListLine(m.msgs[0], false, 60)
	if !strings.Contains(marked, markBG) || !strings.Contains(marked, "✓") {
		t.Errorf("marked row not tinted: %q", marked)
	}
	if cursor := m.renderListLine(m.msgs[1], true, 60); !strings.Contains(cursor, cursorBG) || strings.Contains(cursor, markBG) {
		t.Errorf("cursor row: %q", cursor)
	}
	if both := m.renderListLine(m.msgs[0], true, 60); !strings.Contains(both, markBG) || !strings.Contains(both, "▌") {
		t.Errorf("cursor on a marked row: %q", both)
	}
	// Nothing on a marked row is drawn in the tint's own blue (the unread
	// mark is blue elsewhere): it would vanish.
	if strings.Contains(marked, "34;44m") || strings.Contains(marked, "[34;") {
		t.Errorf("blue on blue: %q", marked)
	}
}
