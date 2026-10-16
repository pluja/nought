// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pluja/nought/internal/ops"
	"github.com/pluja/nought/internal/smtptest"
)

// newSendModel is the test model with an account that sends to a test SMTP
// server, after the default undo window. Timers never fire by themselves:
// tests send flushMsg or quit.
func newSendModel(t *testing.T) (Model, *smtptest.Server) {
	t.Helper()
	m := newTestModel(t, 120, 30)
	srv := smtptest.Start(t, smtptest.Options{})
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(srv.CAPEM)
	m.env.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if _, err := ops.Call(t.Context(), m.env, ops.Delete, ops.AccountAdd, ops.AccountIn{Name: "personal", Email: smtptest.User,
		IMAPHost: "127.0.0.1", Password: smtptest.Password, SMTPHost: srv.Host, SMTPPort: srv.Port}); err != nil {
		t.Fatal(err)
	}
	m.after = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
	return m, srv
}

// fakeEditor edits the draft file like a user would.
func fakeEditor(edit func(text string) string) func(string) tea.Cmd {
	return func(path string) tea.Cmd {
		return func() tea.Msg {
			b, err := os.ReadFile(path)
			if err == nil {
				err = os.WriteFile(path, []byte(edit(string(b))), 0o600)
			}
			return editorDoneMsg{err: err}
		}
	}
}

func drafts(t *testing.T, m Model) []ops.DraftInfo {
	t.Helper()
	list, err := ops.Call(t.Context(), m.env, ops.Read, ops.DraftList, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestReplyReviewSendUndo(t *testing.T) {
	m, srv := newSendModel(t)
	m.editFile = fakeEditor(func(s string) string { return strings.Replace(s, "\n\n", "\n\nSounds good.", 1) })
	m = drive(t, m, key("r"))
	if m.composing == nil || !strings.Contains(screen(m), "Re: Lunch on Friday?") || !strings.Contains(screen(m), "y send") ||
		!strings.Contains(screen(m), "Sounds good.") {
		t.Fatalf("no review screen:\n%s", screen(m))
	}
	m = drive(t, m, key("y"))
	if m.composing != nil || !strings.Contains(screen(m), "in 10s · z undoes") || m.sendingID == "" {
		t.Fatalf("after y:\n%s", screen(m))
	}
	m = drive(t, m, key("z"))
	if m.composing == nil || m.sendingID != "" || len(srv.Messages()) != 0 {
		t.Fatal("z did not bring the message back to review")
	}
	m = drive(t, m, key("q"))
	m = drive(t, m, key("y")) // discard, confirmed
	if m.composing != nil || len(drafts(t, m)) != 0 {
		t.Errorf("discard: composing %v, drafts %d", m.composing != nil, len(drafts(t, m)))
	}
}

func TestChecksBlockSend(t *testing.T) {
	m, _ := newSendModel(t)
	m.editFile = fakeEditor(func(s string) string { return regexp.MustCompile(`(?m)^To:.*$`).ReplaceAllString(s, "To:") })
	m = drive(t, m, key("r"))
	if !strings.Contains(screen(m), "no recipients") {
		t.Fatalf("no blocking check shown:\n%s", screen(m))
	}
	m = drive(t, m, key("y"))
	if m.composing == nil || m.sendingID != "" || !m.noticeErr {
		t.Error("y sent with a blocking problem")
	}
}

func TestEditorFailureKeepsDraft(t *testing.T) { // Review Focus 1
	m, _ := newSendModel(t)
	m.editFile = func(string) tea.Cmd { return func() tea.Msg { return editorDoneMsg{err: errors.New("exit status 1")} } }
	m = drive(t, m, key("c"))
	if !strings.Contains(m.notice, "editor") || m.composing != nil {
		t.Errorf("notice = %q", m.notice)
	}
	if list := drafts(t, m); len(list) != 1 {
		t.Errorf("the draft was lost: %+v", list)
	}
}

func TestFlushReportsSent(t *testing.T) {
	m, srv := newSendModel(t)
	m.editFile = fakeEditor(func(s string) string { return strings.Replace(s, "To: ", "To: bob@example.org", 1) })
	m = drive(t, m, key("c"))
	m = drive(t, m, key("y"))
	m.env.Now = func() time.Time { return testNow.Add(time.Hour) } // the undo window is over
	m = drive(t, m, flushMsg{})
	if len(srv.Messages()) != 1 || m.sendingID != "" || !strings.HasPrefix(m.notice, "sent") {
		t.Errorf("flush: %d sent, notice %q", len(srv.Messages()), m.notice)
	}
}

func TestToCompletesFromCorrespondents(t *testing.T) {
	m, _ := newSendModel(t)
	m.editFile = fakeEditor(func(s string) string { return s + "Hello\n" })
	m = drive(t, m, key("c"))
	m = drive(t, m, key("t"))
	m = drive(t, m, keys("ali")...)
	if len(m.suggestions) == 0 || !strings.Contains(screen(m), "alice@example.org") {
		t.Fatalf("no suggestions:\n%s", screen(m))
	}
	m = drive(t, m, key("tab"), key("enter"))
	if m.composing == nil || len(m.composing.Draft.To) != 1 || m.composing.Draft.To[0] != "alice@example.org" {
		t.Errorf("to = %+v", m.composing)
	}
}

func TestDraftsViewAndQuitFlush(t *testing.T) {
	m, srv := newSendModel(t)
	m.editFile = fakeEditor(func(s string) string { return strings.Replace(s, "To: ", "To: bob@example.org", 1) })
	m = drive(t, m, key("c"))
	m = drive(t, m, key("d")) // keep as draft
	if !strings.Contains(screen(m), "Drafts") {
		t.Errorf("no Drafts in the sidebar:\n%s", screen(m))
	}
	m = drive(t, m, keys("gd")...)
	if !strings.Contains(screen(m), "Drafts") || len(m.drafts) != 1 {
		t.Fatalf("drafts view:\n%s", screen(m))
	}
	m = drive(t, m, key("enter"))
	if m.composing == nil {
		t.Fatal("enter did not resume the draft")
	}
	m = drive(t, m, key("y")) // in its undo window
	m, cmd := m.requestQuit()
	if !strings.Contains(m.notice, "sending") {
		t.Errorf("quit notice = %q", m.notice)
	}
	var quit bool
	for _, msg := range run(cmd) {
		_, cmd := m.Update(msg)
		for _, msg := range run(cmd) {
			_, quit = msg.(tea.QuitMsg)
		}
	}
	if !quit || len(srv.Messages()) != 1 {
		t.Errorf("quit: quit %v, sent %d", quit, len(srv.Messages()))
	}
}

func TestUntouchedNewMessageIsDropped(t *testing.T) {
	m, _ := newSendModel(t)
	m.editFile = fakeEditor(func(s string) string { return s }) // quit without saving
	m = drive(t, m, key("c"))
	if m.composing != nil || !strings.Contains(m.notice, "discarded") || len(drafts(t, m)) != 0 {
		t.Errorf("composing %v, notice %q, drafts %d", m.composing != nil, m.notice, len(drafts(t, m)))
	}
}

func TestRefusedSendIsReported(t *testing.T) {
	m, srv := newSendModel(t)
	srv.SetFailData(550)
	m.editFile = fakeEditor(func(s string) string { return strings.Replace(s, "To: ", "To: bob@example.org", 1) })
	m = drive(t, m, key("c"))
	m = drive(t, m, key("y"))
	m.env.Now = func() time.Time { return testNow.Add(time.Hour) }
	m = drive(t, m, flushMsg{})
	if !m.noticeErr || !strings.Contains(m.notice, "not sent") || m.sendingID != "" {
		t.Errorf("notice %q (err %v), sending %q", m.notice, m.noticeErr, m.sendingID)
	}
}

// Review: mail that went out but could not be archived yet is reported as
// sent (with the problem), never as "not sent", which would invite a resend.
func TestSentButNotArchivedSaysSent(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the test cannot write to")
	}
	m, srv := newSendModel(t)
	m.editFile = fakeEditor(func(s string) string { return strings.Replace(s, "To: ", "To: bob@example.org", 1) })
	m = drive(t, m, key("c"))
	m = drive(t, m, key("y"))
	tmp := filepath.Join(m.env.Paths.AccountArchive("personal"), "tmp")
	if err := os.Chmod(tmp, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o700) })
	m.env.Now = func() time.Time { return testNow.Add(time.Hour) }
	m = drive(t, m, flushMsg{})
	if len(srv.Messages()) != 1 || !strings.HasPrefix(m.notice, "sent: ") || !m.noticeErr {
		t.Errorf("notice %q (err %v), %d received", m.notice, m.noticeErr, len(srv.Messages()))
	}
}

func TestSyncSummaryMentionsSentMail(t *testing.T) {
	r := summarize(ops.SyncOut{Outbox: ops.FlushOut{Sent: 2, Failed: 1}}, nil)
	if s := r.String(); !strings.Contains(s, "sent 2") || r.problems != 1 || !r.worthTelling() {
		t.Errorf("summary %q, problems %d", s, r.problems)
	}
}

// The undo window counts down, and stays on screen while you work.
func TestSendCountsDown(t *testing.T) {
	m, _ := newSendModel(t)
	m.editFile = fakeEditor(func(s string) string { return strings.Replace(s, "To: ", "To: bob@example.org", 1) })
	m = drive(t, m, key("c"))
	m = drive(t, m, key("y"))
	m.now = func() time.Time { return testNow.Add(3 * time.Second) }
	m = drive(t, m, countdownMsg{id: m.sendingID}, key("j")) // a tick, then other work
	if !strings.Contains(screen(m), "in 07s · z undoes") {
		t.Errorf("no countdown:\n%s", screen(m))
	}
	m.now = func() time.Time { return testNow.Add(time.Minute) }
	if _, cmd := m.Update(countdownMsg{id: m.sendingID}); cmd != nil {
		t.Error("the countdown goes on after the window")
	}
}

func TestSecondsLeft(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second: "10s", 8*time.Second + time.Millisecond: "09s", 500 * time.Millisecond: "01s",
		65 * time.Second: "1m 05s", 5 * time.Minute: "5m 00s",
	} {
		if got := secondsLeft(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

// Deferred (d): D discards a draft from the Drafts view, after asking.
func TestDiscardFromTheDraftsView(t *testing.T) {
	m, _ := newSendModel(t)
	m.editFile = fakeEditor(func(s string) string { return strings.Replace(s, "To: ", "To: bob@example.org", 1) })
	m = drive(t, m, key("c"))
	m = drive(t, m, key("d")) // keep as draft
	m = drive(t, m, keys("gd")...)
	m = drive(t, m, key("D"))
	if !strings.Contains(screen(m), "Discard this draft?") {
		t.Fatalf("no confirmation:\n%s", screen(m))
	}
	m = drive(t, m, key("n"))
	if len(drafts(t, m)) != 1 {
		t.Fatal("discarded without a yes")
	}
	m = drive(t, m, key("D"))
	m = drive(t, m, key("y"))
	if len(drafts(t, m)) != 0 || len(m.drafts) != 0 {
		t.Errorf("drafts left: %d (view %d)", len(drafts(t, m)), len(m.drafts))
	}
}
