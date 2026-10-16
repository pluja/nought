// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pluja/nought/internal/ops"
)

// Regressions found by the whole-codebase review: each test pins one.

func unreadCount(t *testing.T, m Model) int {
	t.Helper()
	out, err := ops.Call(t.Context(), m.env, ops.Read, ops.MsgCount, ops.CountIn{Query: "is:unread"})
	if err != nil {
		t.Fatal(err)
	}
	return out.Count
}

// One Enter in the Unread view marked every unread message read: each
// opened message left the view, which opened (and marked read) the next.
func TestOpeningInUnreadMarksOneRead(t *testing.T) {
	m := newTestModel(t, 80, 24)
	before := unreadCount(t, m)
	m = drive(t, m, keys("gu")...)
	m = drive(t, m, key("enter"))
	if got := unreadCount(t, m); got != before-1 {
		t.Errorf("unread %d → %d after one Enter", before, got)
	}
}

// A refresh dropping the open message (archived on another device) moves
// on, without marking the next message read.
func TestRefreshDoesNotMarkTheNextRead(t *testing.T) {
	m := newTestModel(t, 80, 24)
	m = drive(t, m, key("enter")) // reading the first message
	before := unreadCount(t, m)
	m = drive(t, m, searchDoneMsg{gen: m.searchGen, out: ops.SearchOut{Messages: m.msgs[1:]}})
	if got := unreadCount(t, m); got != before {
		t.Errorf("unread %d → %d after a refresh", before, got)
	}
}

// Notices (errors above all) were hidden behind "syncing…".
func TestNoticesShowDuringSync(t *testing.T) {
	m := newTestModel(t, 80, 24)
	m, _ = m.startSync(false)
	m.syncCancel = func() {}
	m = drive(t, m, key("q"))
	if !strings.Contains(screen(m), "stopping the sync") {
		t.Errorf("quit notice hidden:\n%s", screen(m))
	}
}

// Mail another nou (or a sync) sent first left "sending…" up forever.
func TestSendingClearsWhenSentElsewhere(t *testing.T) {
	m, srv := newSendModel(t)
	m.editFile = fakeEditor(func(s string) string { return strings.Replace(s, "To: ", "To: bob@example.org", 1) })
	m = drive(t, m, key("c"))
	m = drive(t, m, key("y"))
	m.env.Now = func() time.Time { return testNow.Add(time.Hour) }
	if _, err := ops.Call(t.Context(), m.env, ops.Send, ops.OutboxFlush, ops.FlushIn{}); err != nil {
		t.Fatal(err)
	}
	m = drive(t, m, flushMsg{})
	if m.sendingID != "" || len(srv.Messages()) != 1 || !strings.HasPrefix(m.notice, "sent") {
		t.Errorf("sendingID %q, notice %q", m.sendingID, m.notice)
	}
}

// Enter in the link box opened the tracked link: only its listed keys act.
func TestLinkBoxIgnoresEnter(t *testing.T) {
	m := withLinksMessage(t, newTestModel(t, 120, 30))
	var opened []string
	m.openURL = func(u string) error { opened = append(opened, u); return nil }
	m = drive(t, m, key("o"), key("1"), key("enter"))
	if len(opened) != 0 {
		t.Errorf("Enter opened %v", opened)
	}
}

// Reader actions act on the message on screen, not on the list cursor
// (which J moves before the next message has loaded).
func TestReaderActsOnTheMessageShown(t *testing.T) {
	m := newTestModel(t, 80, 24)
	m = drive(t, m, key("enter"))
	shown := m.open.Message.ID
	next, _ := m.Update(key("J")) // the next message is still loading
	m = next.(Model)
	next, _ = m.Update(key("s"))
	m = next.(Model)
	if len(m.tagQueue) == 0 && !m.tagBusy {
		t.Fatal("s did nothing")
	}
	var flagged string
	for _, msg := range m.msgs {
		if slicesContains(msg.Tags, ops.TagFlagged) {
			flagged = msg.ID
		}
	}
	if flagged != shown {
		t.Errorf("flagged %q, the reader shows %q", flagged, shown)
	}
}

// z right after an action undid the previous action (its save had not
// finished); quitting did not wait for queued saves either.
func TestUndoAndQuitWaitForSaves(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("s")) // flag the first message; saved
	m = drive(t, m, key("j"))
	second := m.msgs[m.cursor]
	next, save := m.Update(key("e")) // archive the second; its save is pending
	m = next.(Model)
	m = drive(t, m, key("z"))     // waits for the save
	m = drive(t, m, run(save)...) // the save lands, then the undo runs
	if !strings.Contains(m.notice, "undid archived") {
		t.Errorf("notice %q", m.notice)
	}
	all, err := ops.Call(t.Context(), m.env, ops.Read, ops.MsgSearch, ops.SearchIn{Query: "hash:" + second.ID})
	if err != nil || len(all.Messages) != 1 || !slicesContains(all.Messages[0].Tags, ops.TagInbox) {
		t.Errorf("z did not undo the archive: %+v, %v", all.Messages, err)
	}

	m = newTestModel(t, 120, 30)
	next, _ = m.Update(key("e"))
	m = next.(Model)
	if _, cmd := m.Update(key("q")); cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Error("q quit with a save in flight")
		}
	}
}

// A tag prompt applies to the message it was opened on, even if a refresh
// moved the cursor meanwhile.
func TestTagPromptKeepsItsMessage(t *testing.T) {
	m := newTestModel(t, 120, 30)
	first := m.msgs[0]
	m = drive(t, m, key("l"))
	m = drive(t, m, searchDoneMsg{gen: m.searchGen, out: ops.SearchOut{Messages: m.msgs[1:]}})
	m = drive(t, m, append(keys("+work"), key("enter"))...)
	got, err := ops.Call(t.Context(), m.env, ops.Read, ops.MsgSearch, ops.SearchIn{Query: "tag:work"})
	if err != nil || len(got.Messages) != 1 || got.Messages[0].ID != first.ID {
		t.Errorf("tagged %+v, want %s", got.Messages, first.Subject)
	}
}

// Two crashes: a key in the link box after the links changed (v), and a key
// in the To input after the review screen closed (y).
func TestNoPanics(t *testing.T) {
	m := withLinksMessage(t, newTestModel(t, 120, 30))
	m.linkPick = 2
	m.links = nil
	m = drive(t, m, key("x"))
	if m.linkPick != 0 {
		t.Error("link box left open with no links")
	}

	s, _ := newSendModel(t)
	s.editFile = fakeEditor(func(text string) string { return strings.Replace(text, "To: ", "To: bob@example.org", 1) })
	s = drive(t, s, key("c"))
	s = drive(t, s, key("t"))
	s.composing = nil // the review closed (sent) while the To input was open
	s = drive(t, s, key("a"))
	if s.inputKind != inputNone {
		t.Error("To input left open without a draft")
	}
}

// Tab does not focus a sidebar too narrow to show.
func TestTabSkipsAHiddenSidebar(t *testing.T) {
	m := newTestModel(t, 50, 20)
	m = drive(t, m, key("tab"))
	if m.focus == focusSidebar {
		t.Error("focused an invisible sidebar")
	}
}

// The save prompt stays visible, and a failed search keeps the list rather
// than claiming Inbox Zero.
func TestPromptsAndErrorsStayVisible(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m.inputKind = inputSaveDir
	m.open = nil
	if !strings.Contains(m.renderPromptLine(), "save") {
		t.Errorf("save prompt invisible: %q", m.renderPromptLine())
	}
	m = newTestModel(t, 120, 30)
	m = drive(t, m, searchDoneMsg{gen: m.searchGen, err: errors.New("database is locked")})
	if s := screen(m); strings.Contains(s, "Inbox Zero") || !strings.Contains(s, "The search failed") {
		t.Errorf("a failed search claimed Inbox Zero:\n%s", s)
	}
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
