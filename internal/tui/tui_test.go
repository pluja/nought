// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/ops"
	"github.com/pluja/nought/internal/secret"
)

var update = flag.Bool("update", false, "update golden files")

var testNow = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// TestMain pins the time zone, so rendered dates (and golden files) do not
// depend on where the tests run.
func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}

// newTestModel returns a model over a fresh archive with a few messages.
func newTestModel(t *testing.T, w, h int) Model {
	t.Helper()
	home := t.TempDir()
	paths, err := config.ResolvePaths("linux", func(k string) string {
		if k == "NOUGHT_HOME" {
			return home
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	env := ops.NewEnv(paths, slog.New(slog.NewTextHandler(io.Discard, nil)), func() time.Time { return testNow },
		ops.BuildInfo{}, secret.Memory{})
	t.Cleanup(func() { _ = env.Close() })

	export := filepath.Join(t.TempDir(), "export", "Inbox")
	msgs := map[string]string{
		"a.eml": mail("a@x", "Alice <alice@example.org>", "Lunch on Friday?", "Wed, 14 Oct 2026 09:00:00 +0000", "Pizza at noon?"),
		"b.eml": mail("b@x", "Bob <bob@example.org>", "Invoice 2026-114", "Tue, 13 Oct 2026 09:00:00 +0000", "Total: 120 EUR"),
		"c.eml": mail("c@x", "Mallory <m@example.org>", "=?UTF-8?Q?evil_=1B[31mred?=", "Mon, 12 Oct 2026 09:00:00 +0000", "gotcha\x1b]0;pwned\x07"),
	}
	for name, raw := range msgs {
		if err := os.MkdirAll(export, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(export, name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ops.Call(t.Context(), env, ops.Delete, ops.ImportRun,
		ops.ImportIn{Path: filepath.Dir(export), Account: "personal", Inbox: true}); err != nil {
		t.Fatal(err)
	}
	m := newModel(t.Context(), env)
	m.previewWait = 0
	// No cursor blink: its timer would never let drive finish.
	styles := m.input.Styles()
	styles.Cursor.Blink = false
	m.input.SetStyles(styles)
	return drive(t, m, tea.WindowSizeMsg{Width: w, Height: h}, initMsg{})
}

func mail(id, from, subject, date, body string) string {
	return "From: " + from + "\r\nTo: me@example.org\r\nMessage-ID: <" + id + ">\r\nDate: " + date +
		"\r\nSubject: " + subject + "\r\n\r\n" + body + "\r\n"
}

// initMsg asks drive to run the model's Init.
type initMsg struct{}

// drive feeds messages to the model and runs the resulting commands to
// completion (commands that do not finish quickly, like cursor blinks, are
// dropped).
func drive(t *testing.T, m Model, msgs ...tea.Msg) Model {
	t.Helper()
	queue := slices.Clone(msgs)
	for steps := 0; len(queue) > 0; steps++ {
		if steps > 500 {
			t.Fatal("too many steps")
		}
		msg := queue[0]
		queue = queue[1:]
		var cmd tea.Cmd
		if _, ok := msg.(initMsg); ok {
			cmd = m.Init()
		} else {
			var next tea.Model
			next, cmd = m.Update(msg)
			m = next.(Model)
		}
		queue = append(queue, run(cmd)...)
	}
	return m
}

// commandTimeout drops commands that wait for a long time (timers such as
// the sync schedule). Real work (index, store) must finish within it even
// on slow CI runners with -race and coverage.
const commandTimeout = 5 * time.Second

func run(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			var out []tea.Msg
			for _, c := range batch {
				out = append(out, run(c)...)
			}
			return out
		}
		if msg == nil {
			return nil
		}
		return []tea.Msg{msg}
	case <-time.After(commandTimeout):
		return nil
	}
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

func keys(s string) []tea.Msg {
	var out []tea.Msg
	for _, r := range s {
		out = append(out, key(string(r)))
	}
	return out
}

func screen(m Model) string { return ansi.Strip(m.render()) }

func TestOpensOnTheInbox(t *testing.T) {
	m := newTestModel(t, 120, 30)
	s := screen(m)
	for _, want := range []string{"nought  Inbox  3 messages", "Lunch on Friday?", "Invoice 2026-114",
		"Inbox", "no accounts", "? more"} {
		if !strings.Contains(s, want) {
			t.Errorf("screen lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(m.render(), "\x1b]0;") || strings.Contains(s, "[31m") {
		t.Error("hostile escape sequence reached the screen")
	}
	if !strings.Contains(s, "evil red") {
		t.Errorf("sanitized subject missing:\n%s", s)
	}
}

func TestOpenMarksReadAndShowsLocation(t *testing.T) {
	m := newTestModel(t, 120, 30)
	if !slices.Contains(m.msgs[0].Tags, ops.TagUnread) {
		t.Fatal("expected an unread message")
	}
	m = drive(t, m, key("enter"))
	if m.focus != focusReader || m.open == nil || m.open.Message.Subject != "Lunch on Friday?" {
		t.Fatalf("focus %v, open %+v", m.focus, m.open)
	}
	if slices.Contains(m.msgs[0].Tags, ops.TagUnread) {
		t.Error("opening did not mark the message read")
	}
	s := screen(m)
	for _, want := range []string{"Pizza at noon?", "● Local only"} {
		if !strings.Contains(s, want) {
			t.Errorf("reader lacks %q:\n%s", want, s)
		}
	}
	m = drive(t, m, key("esc"))
	if m.focus != focusList {
		t.Error("esc did not return to the list")
	}
}

func TestArchiveAndUndo(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("e"))
	if len(m.msgs) != 2 || !strings.Contains(m.notice, "archived") {
		t.Fatalf("after archive: %d messages, notice %q", len(m.msgs), m.notice)
	}
	m = drive(t, m, key("z"))
	if len(m.msgs) != 3 || !strings.Contains(m.notice, "undid archived: Lunch on Friday?") {
		t.Fatalf("after undo: %d messages, notice %q", len(m.msgs), m.notice)
	}
	m = drive(t, m, key("z"))
	if m.notice != "nothing to undo" {
		t.Errorf("notice = %q", m.notice)
	}
}

func TestToggleFlagAndTags(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("s"))
	if !slices.Contains(m.msgs[0].Tags, ops.TagFlagged) {
		t.Fatal("not flagged")
	}
	m = drive(t, m, append([]tea.Msg{key("l")}, append(keys("+todo -flagged"), key("enter"))...)...)
	if !slices.Contains(m.msgs[0].Tags, "todo") || slices.Contains(m.msgs[0].Tags, ops.TagFlagged) {
		t.Errorf("tags = %v", m.msgs[0].Tags)
	}
	m = drive(t, m, key("u"), key("u"))
	if !slices.Contains(m.msgs[0].Tags, ops.TagUnread) {
		t.Errorf("u twice should leave it unread: %v", m.msgs[0].Tags)
	}
}

func TestSearchAndViews(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, append([]tea.Msg{key("/")}, append(keys("from:bob"), key("enter"))...)...)
	if m.query != "from:bob" || len(m.msgs) != 1 || m.msgs[0].Subject != "Invoice 2026-114" {
		t.Fatalf("search: %q, %d", m.query, len(m.msgs))
	}
	m = drive(t, m, key("g"), key("i"))
	if m.query != queryInbox || len(m.msgs) != 3 {
		t.Errorf("gi: %q, %d", m.query, len(m.msgs))
	}
	m = drive(t, m, key("G"))
	if m.cursor != 2 {
		t.Errorf("G: cursor %d", m.cursor)
	}
	m = drive(t, m, key("g"), key("g"))
	if m.cursor != 0 {
		t.Errorf("gg: cursor %d", m.cursor)
	}
	m = drive(t, m, append([]tea.Msg{key("/")}, append(keys("bogus:x"), key("enter"))...)...)
	if !strings.Contains(m.notice, "unknown field") {
		t.Errorf("bad search notice = %q", m.notice)
	}
}

func TestSidebarNavigation(t *testing.T) {
	m := newTestModel(t, 120, 30)
	if len(m.side) < 4 || m.side[0].count != 3 {
		t.Fatalf("sidebar = %+v", m.side)
	}
	m = drive(t, m, key("tab"))
	if m.focus != focusReader && m.focus != focusSidebar {
		t.Fatalf("tab focus = %v", m.focus)
	}
	m.focus = focusSidebar
	m = drive(t, m, key("j"), key("enter"))
	if m.query != queryUnread || m.focus != focusList {
		t.Errorf("sidebar enter: query %q focus %v", m.query, m.focus)
	}
	m = drive(t, m, key("b"))
	if m.sidebar || strings.Contains(screen(m), "Flagged") {
		t.Error("sidebar still shown")
	}
}

func TestNarrowLayoutOpensFullScreen(t *testing.T) {
	m := newTestModel(t, 80, 24)
	m = drive(t, m, key("enter"))
	s := screen(m)
	if !strings.Contains(s, "Pizza at noon?") || strings.Contains(s, "Invoice") {
		t.Errorf("narrow reader should cover the list:\n%s", s)
	}
	m = drive(t, m, key("q"))
	if m.focus != focusList {
		t.Error("q in the narrow reader should go back, not quit")
	}
}

func TestPaletteAndHelp(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, append([]tea.Msg{key(":")}, append(keys("bogus"), key("enter"))...)...)
	if !strings.Contains(m.notice, "unknown command") {
		t.Errorf("notice = %q", m.notice)
	}
	m = drive(t, m, append([]tea.Msg{key(":")}, append(keys("search from:alice"), key("enter"))...)...)
	if len(m.msgs) != 1 {
		t.Errorf("palette search: %d", len(m.msgs))
	}
	m = drive(t, m, key("?"))
	if !strings.Contains(screen(m), "What happens where") || !m.help {
		t.Error("help not shown")
	}
	m = drive(t, m, key("x"))
	if m.help {
		t.Error("help not closed")
	}
}

func TestLocationLinesAndMarkers(t *testing.T) {
	soon := testNow.Add(72 * time.Hour)
	past := testNow.Add(-time.Hour)
	tests := []struct {
		server []ops.ServerLocation
		want   string
		marker string
	}{
		{nil, "Local only", "●"},
		{[]ops.ServerLocation{{Folder: "INBOX"}}, "On server · INBOX on personal", "◐"},
		{[]ops.ServerLocation{{Folder: "INBOX", LeavesAt: &soon}}, "Leaves the server in 3 days · INBOX", "◕"},
		{[]ops.ServerLocation{{Folder: "INBOX", LeavesAt: &past}}, "at the next sync", "◕"},
	}
	m := Model{now: func() time.Time { return testNow }, st: newStyles()}
	for _, tt := range tests {
		msg := ops.MessageOut{Account: "personal", Server: tt.server}
		if got := strings.Join(locationLines(msg, testNow), " "); !strings.Contains(got, tt.want) {
			t.Errorf("location %+v = %q, want %q", tt.server, got, tt.want)
		}
		if got := m.marker(msg); got != tt.marker {
			t.Errorf("marker %+v = %q, want %q", tt.server, got, tt.marker)
		}
	}
	m.prefs.ASCII = true
	if got := m.marker(ops.MessageOut{Server: []ops.ServerLocation{{LeavesAt: &soon}}}); got != "-" {
		t.Errorf("ascii marker = %q", got)
	}
}

func TestParseTagInput(t *testing.T) {
	add, remove := parseTagInput("+todo -Inbox work + -")
	if !slices.Equal(add, []string{"todo", "work"}) || !slices.Equal(remove, []string{"inbox"}) {
		t.Errorf("add %v remove %v", add, remove)
	}
}

func TestGoldenScreens(t *testing.T) {
	cases := map[string]func(*testing.T) Model{
		"list-120x30": func(t *testing.T) Model { return newTestModel(t, 120, 30) },
		"reader-120x30": func(t *testing.T) Model {
			return drive(t, newTestModel(t, 120, 30), key("j"), key("enter"))
		},
		"list-80x24": func(t *testing.T) Model { return newTestModel(t, 80, 24) },
		"help-80x24": func(t *testing.T) Model { return drive(t, newTestModel(t, 80, 24), key("?")) },
		"review-120x30": func(t *testing.T) Model {
			m, _ := newSendModel(t)
			m.editFile = fakeEditor(func(s string) string {
				return strings.Replace(s, "\n\n", "\n\nSounds good, see you there.\n\nAttached is the menu.", 1)
			})
			return drive(t, m, key("r"))
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			got := screen(build(t)) + "\n"
			golden := filepath.Join("testdata", name+".golden")
			if *update {
				if err := os.MkdirAll("testdata", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("%s changed:\n%s\nwant:\n%s", name, got, want)
			}
		})
	}
}

func TestStaleResultsAreIgnored(t *testing.T) {
	m := newTestModel(t, 120, 30)
	before := len(m.msgs)
	// A search result from an older request must not replace the view.
	m = drive(t, m, searchDoneMsg{gen: m.searchGen - 1, out: ops.SearchOut{}})
	if len(m.msgs) != before {
		t.Fatalf("stale search applied: %d messages", len(m.msgs))
	}
	m = drive(t, m, showDoneMsg{gen: m.showGen - 1, id: m.msgs[0].ID, out: ops.ShowOut{Body: "stale"}})
	if m.open != nil && m.open.Body == "stale" {
		t.Error("stale show applied")
	}
}

func TestCursorFollowsTheMessage(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("j")) // Bob
	bob := m.msgs[m.cursor].ID
	// New mail arrives at the top; the same message stays selected.
	newer := ops.MessageOut{ID: "000000000000", Account: "personal", Subject: "new", Tags: []string{"inbox"}}
	m = drive(t, m, searchDoneMsg{gen: m.searchGen, out: ops.SearchOut{Messages: append([]ops.MessageOut{newer}, m.msgs...)}})
	if m.msgs[m.cursor].ID != bob {
		t.Errorf("cursor moved to %q", m.msgs[m.cursor].Subject)
	}
}

func TestArchiveWhileReadingOpensTheNext(t *testing.T) {
	m := newTestModel(t, 80, 24)
	m = drive(t, m, key("enter"), key("e"))
	if m.focus != focusReader || m.open == nil || m.open.Message.Subject != "Invoice 2026-114" {
		t.Fatalf("after archiving in the reader: focus %v, open %+v", m.focus, m.open)
	}
	m = drive(t, m, key("e"), key("e"))
	if len(m.msgs) != 0 || m.open != nil || m.focus != focusList {
		t.Errorf("empty inbox: %d messages, open %v, focus %v", len(m.msgs), m.open != nil, m.focus)
	}
	if !strings.Contains(screen(m), "Inbox Zero") {
		t.Error("no Inbox Zero message")
	}
}

func TestSearchErrorClearsTheList(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, append([]tea.Msg{key("/")}, append(keys("bogus:x"), key("enter"))...)...)
	if len(m.msgs) != 0 || m.open != nil {
		t.Errorf("old results kept under a failed search: %d", len(m.msgs))
	}
}

func TestGPrefix(t *testing.T) {
	m := newTestModel(t, 80, 24)
	m = drive(t, m, key("g"), key("j"))
	if m.cursor != 1 {
		t.Errorf("g then j: cursor %d (j was swallowed)", m.cursor)
	}
	m = drive(t, m, key("enter"), key("g"), key("u"))
	if m.focus != focusList || m.query != queryUnread || m.open != nil {
		t.Errorf("gu from the reader: focus %v query %q", m.focus, m.query)
	}
}

func TestSyncRunsWithoutAsking(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m.status = []ops.AccountStatusOut{{Account: "personal", Deletion: true}}
	next, cmd := m.Update(key("S"))
	m = next.(Model)
	if !m.syncing || m.syncAuto || cmd == nil || m.inputKind != inputNone {
		t.Fatalf("S: syncing %v auto %v cmd %v input %v", m.syncing, m.syncAuto, cmd != nil, m.inputKind)
	}

	// Quitting while a sync runs stops it first; a second request forces.
	cancelled := false
	m.syncing, m.syncCancel = true, func() { cancelled = true }
	next, cmd = m.Update(key("q"))
	m = next.(Model)
	if cmd != nil || !m.quitting || !cancelled {
		t.Fatalf("quit during sync: cmd %v quitting %v cancelled %v", cmd != nil, m.quitting, cancelled)
	}
	if _, cmd := m.Update(syncDoneMsg{}); cmd == nil {
		t.Error("no quit after the sync stopped")
	}
}

func TestNextSyncDelay(t *testing.T) {
	for _, c := range []struct {
		every    time.Duration
		failures int
		want     time.Duration
	}{
		{10 * time.Minute, 0, 10 * time.Minute},
		{10 * time.Minute, 1, 20 * time.Minute},
		{10 * time.Minute, 3, time.Hour},
		{10 * time.Minute, 1000, time.Hour},
		{2 * time.Hour, 0, 2 * time.Hour},
		{2 * time.Hour, 5, 2 * time.Hour},
	} {
		if got := nextSyncDelay(c.every, c.failures); got != c.want {
			t.Errorf("nextSyncDelay(%v, %d) = %v, want %v", c.every, c.failures, got, c.want)
		}
	}
}

func TestAutoSyncSchedule(t *testing.T) {
	m := newTestModel(t, 120, 30)
	if m.syncEvery != 0 {
		t.Fatal("no accounts: auto-sync must be off")
	}
	m.syncEvery = 10 * time.Minute

	// A tick from an old schedule does nothing; the current one syncs.
	m, _ = m.autoSync(autoSyncMsg{gen: m.syncSchedule - 1})
	if m.syncing {
		t.Fatal("stale tick started a sync")
	}
	m, cmd := m.autoSync(autoSyncMsg{gen: m.syncSchedule})
	if !m.syncing || !m.syncAuto || cmd == nil {
		t.Fatal("tick did not start an automatic sync")
	}
	if again, _ := m.autoSync(autoSyncMsg{gen: m.syncSchedule}); again.notice == "already syncing" {
		t.Error("a tick during a sync must stay quiet")
	}

	// Nothing new: quiet, and a new schedule starts.
	m.notice = "earlier"
	gen := m.syncSchedule
	m, _ = m.syncDone(syncDoneMsg{})
	if m.notice != "earlier" || m.syncSchedule != gen+1 || m.syncFailures != 0 {
		t.Errorf("quiet sync: notice %q schedule %d failures %d", m.notice, m.syncSchedule, m.syncFailures)
	}

	// Failures are told and back off; another sync running is not a failure.
	m.syncing, m.syncAuto = true, true
	m, _ = m.syncDone(syncDoneMsg{err: errors.New("offline")})
	if m.syncFailures != 1 || !m.noticeErr || !strings.Contains(m.notice, "1 problems") {
		t.Errorf("failed sync: failures %d notice %q", m.syncFailures, m.notice)
	}
	m.syncing, m.syncAuto = true, true
	m, _ = m.syncDone(syncDoneMsg{err: ops.ErrBusy})
	if m.syncFailures != 1 {
		t.Errorf("busy counted as a failure: %d", m.syncFailures)
	}
	m.syncing, m.syncAuto = true, true
	m, _ = m.syncDone(syncDoneMsg{out: ops.SyncOut{Accounts: []ops.AccountSyncOut{{Folders: []ops.FolderSyncOut{{Fetched: 2, Expunged: 1}}}}}})
	if m.syncFailures != 0 || m.notice != "synced: 2 new, 1 left the server" {
		t.Errorf("news: failures %d notice %q", m.syncFailures, m.notice)
	}
}

func TestTinyTerminals(t *testing.T) {
	m := newTestModel(t, 120, 30)
	for _, size := range [][2]int{{1, 1}, {10, 2}, {20, 3}, {40, 5}, {59, 10}, {99, 20}, {200, 60}} {
		m = drive(t, m, tea.WindowSizeMsg{Width: size[0], Height: size[1]}, key("j"), key("enter"))
		got := strings.Split(m.render(), "\n")
		if len(got) != size[1] {
			t.Errorf("%dx%d: rendered %d lines", size[0], size[1], len(got))
		}
		for i, line := range got {
			if w := ansi.StringWidth(line); w > size[0] {
				t.Errorf("%dx%d: line %d is %d wide", size[0], size[1], i, w)
			}
		}
		m = drive(t, m, key("esc"))
	}
}

func TestListShowsStateAndTags(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("s"), key("l"))
	m = drive(t, m, append(keys("+todo"), key("enter"))...)
	s := screen(m)
	for _, want := range []string{"★", "◆", "#todo", "●"} {
		if !strings.Contains(s, want) {
			t.Errorf("list lacks %q:\n%s", want, s)
		}
	}
	row := m.renderListLine(m.msgs[0], false, 200)
	if strings.Contains(row, "#inbox") || strings.Contains(row, "#folder:") || strings.Contains(row, "#unread") {
		t.Errorf("view-implied tags shown in the list row: %q", ansi.Strip(row))
	}
	if !strings.Contains(m.render(), "\x1b[") {
		t.Error("no color in the output")
	}
}

func TestReadRowsAreDimmed(t *testing.T) {
	m := newTestModel(t, 120, 30)
	unread := m.renderListLine(m.msgs[1], false, 120)
	m = drive(t, m, key("j"), key("u")) // mark the second one read
	read := m.renderListLine(m.msgs[1], false, 120)
	if !strings.Contains(unread, "\x1b[1mBob") || !strings.Contains(unread, "◆") {
		t.Errorf("unread row not bold with a marker: %q", unread)
	}
	if !strings.Contains(read, "\x1b[2mBob") || strings.Contains(read, "◆") {
		t.Errorf("read row not dimmed: %q", read)
	}
}

func TestSenderShowsDomain(t *testing.T) {
	if got := senderName("PayPal <alert@evil.example>"); got != "PayPal (evil.example)" {
		t.Errorf("senderName = %q", got)
	}
	if got := senderName("plain@example.org"); got != "plain@example.org" {
		t.Errorf("senderName = %q", got)
	}
}

// withLinksMessage adds an HTML message with links to the test archive and
// opens it.
func withLinksMessage(t *testing.T, m Model) Model {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "x", "Inbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := "From: News <news@example.org>\r\nMessage-ID: <links@x>\r\nDate: Thu, 15 Oct 2026 09:00:00 +0000\r\n" +
		"Subject: With links\r\nContent-Type: text/html\r\n\r\n" +
		`<p>Read <a href="https://example.org/post?track=123&utm_source=news">the post</a> or log in at ` +
		`<a href="https://evil.example/login">paypal.com</a>.</p>` + "\r\n"
	if err := os.WriteFile(filepath.Join(dir, "l.eml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Call(t.Context(), m.env, ops.Delete, ops.ImportRun,
		ops.ImportIn{Path: filepath.Dir(dir), Account: "personal", Inbox: true}); err != nil {
		t.Fatal(err)
	}
	next, _ := m.refresh()
	m = drive(t, next, append([]tea.Msg{}, run(next.search(next.query, next.searchGen))...)...)
	m = drive(t, m, key("g"), key("g"), key("enter"))
	if m.open == nil || m.open.Message.Subject != "With links" {
		t.Fatalf("open = %+v", m.open)
	}
	return m
}

func TestLinksCollapsedAndToggled(t *testing.T) {
	m := withLinksMessage(t, newTestModel(t, 120, 30))
	s := screen(m)
	if !strings.Contains(s, "the post[1]") || !strings.Contains(s, "2 links · o open · L show") || strings.Contains(s, "track=123") ||
		strings.Contains(s, "HTML shown as text") {
		t.Fatalf("collapsed links:\n%s", s)
	}
	m = drive(t, m, key("L"))
	s = screen(m)
	if !strings.Contains(s, "[1] example.org https://example.org/") || !strings.Contains(s, "[2] ! evil.example") {
		t.Errorf("link list:\n%s", s)
	}
	m = drive(t, m, key("L"))
	if strings.Contains(screen(m), "track=123") {
		t.Error("list not hidden again")
	}
}

func TestOpenLinkAsksFirst(t *testing.T) {
	m := withLinksMessage(t, newTestModel(t, 120, 30))
	var opened []string
	m.openURL = func(u string) error { opened = append(opened, u); return nil }

	m = drive(t, m, key("o"), key("2"))
	s := screen(m)
	if m.linkPick != 2 || !strings.Contains(s, "Goes to: evil.example") || !strings.Contains(s, "different site") ||
		!strings.Contains(s, "tells the sender you clicked") {
		t.Fatalf("confirmation box:\n%s", s)
	}
	if len(opened) != 0 {
		t.Fatal("opened before confirming")
	}
	m = drive(t, m, key("esc"))
	if m.linkPick != 0 || len(opened) != 0 || m.notice != "link not opened" {
		t.Errorf("cancel: pick %d opened %v notice %q", m.linkPick, opened, m.notice)
	}

	m = drive(t, m, key("o"), key("1"))
	if s := screen(m); !strings.Contains(s, "Without tracking") || !strings.Contains(s, "removed: utm_source") {
		t.Errorf("no clean option:\n%s", s)
	}
	m = drive(t, m, key("o"), key("o"), key("1"), key("O"))
	if len(opened) != 2 || opened[0] != "https://example.org/post?track=123&utm_source=news" ||
		opened[1] != "https://example.org/post?track=123" {
		t.Errorf("opened = %v", opened)
	}

	m = drive(t, m, key("o"), key("1"))
	next, cmd := m.Update(key("c"))
	m = next.(Model)
	if cmd == nil || m.linkPick != 0 || !strings.Contains(m.notice, "copied") {
		t.Errorf("copy: cmd %v pick %d notice %q", cmd != nil, m.linkPick, m.notice)
	}

	m = drive(t, m, key("o"), key("9"))
	if m.linkPick != 0 || !strings.Contains(m.notice, "no link 9") {
		t.Errorf("bad number: pick %d notice %q", m.linkPick, m.notice)
	}
	// With 2 links, a single digit is enough: no Enter.
	m = drive(t, m, key("o"), key("2"))
	if m.linkPick != 2 {
		t.Errorf("o2 did not open the box at once: pick %d", m.linkPick)
	}
}

func TestOpenableSchemes(t *testing.T) {
	for link, want := range map[string]bool{
		"https://x.example": true, "http://x.example": true, "mailto:a@x.example": true,
		"javascript:alert(1)": false, "file:///etc/passwd": false, "x": false,
	} {
		if got := openable(link); got != want {
			t.Errorf("openable(%q) = %v", link, got)
		}
	}
	if err := openInBrowser("file:///etc/passwd"); err == nil {
		t.Error("opened a file: URL")
	}
}

func TestLinkNumberComplete(t *testing.T) {
	for _, tt := range []struct {
		typed string
		links int
		want  bool
	}{
		{"3", 9, true}, {"3", 25, true}, {"2", 25, false}, {"24", 25, true}, {"1", 10, false},
		{"10", 10, true}, {"1", 150, false}, {"15", 150, false}, {"150", 150, true}, {"x", 5, false}, {"0", 5, false},
	} {
		if got := linkNumberComplete(tt.typed, tt.links); got != tt.want {
			t.Errorf("linkNumberComplete(%q, %d) = %v", tt.typed, tt.links, got)
		}
	}
}

func TestHintsFollowTheScreen(t *testing.T) {
	m := newTestModel(t, 120, 30)
	if s := screen(m); !strings.Contains(s, "e archive") || !strings.Contains(s, "/ search") {
		t.Errorf("list hints missing:\n%s", s)
	}
	m = drive(t, m, key("enter"))
	if s := screen(m); !strings.Contains(s, "o link") || !strings.Contains(s, "J/K next") {
		t.Errorf("reader hints missing:\n%s", s)
	}
	off := false
	m.prefs.Hints = &off
	if strings.Contains(screen(m), "o link") {
		t.Error("hints shown although disabled")
	}
}

func TestTrashRestoreAndUndo(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("#"))
	if len(m.msgs) != 2 || m.notice != "moved to the trash" {
		t.Fatalf("after trash: %d messages, notice %q", len(m.msgs), m.notice)
	}
	if !slices.ContainsFunc(m.side, func(s sideItem) bool { return s.label == "Trash" && s.count == 1 }) {
		t.Errorf("sidebar has no Trash (1): %+v", m.side)
	}
	m = drive(t, m, key("z"))
	if len(m.msgs) != 3 || slices.ContainsFunc(m.side, func(s sideItem) bool { return s.label == "Trash" }) {
		t.Fatalf("after undo: %d messages, sidebar %+v", len(m.msgs), m.side)
	}

	// Trash again, then restore it from the trash view.
	m = drive(t, m, key("#"))
	m = drive(t, m, keys("gt")...)
	if m.query != queryTrash || len(m.msgs) != 1 {
		t.Fatalf("trash view: query %q, %d messages", m.query, len(m.msgs))
	}
	m = drive(t, m, key("enter"))
	if s := screen(m); !strings.Contains(s, "In the trash until 14 Nov 2026") || strings.Contains(s, "#deleted") {
		t.Errorf("reader:\n%s", s)
	}
	m = drive(t, m, key("#"))
	if len(m.msgs) != 0 || m.notice != "restored" {
		t.Fatalf("after restore: %d messages, notice %q", len(m.msgs), m.notice)
	}
	m = drive(t, m, keys("gi")...)
	if len(m.msgs) != 3 || slices.ContainsFunc(m.msgs[0].Tags, hiddenTag) {
		t.Fatalf("inbox after restore: %+v", m.msgs)
	}
}

func TestDeleteNowAsksFirst(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("D"))
	if !strings.Contains(screen(m), "Delete permanently") {
		t.Fatalf("no confirmation:\n%s", screen(m))
	}
	m = drive(t, m, key("n"))
	if len(m.msgs) != 3 || m.notice != "not deleted" {
		t.Fatalf("after no: %d messages, notice %q", len(m.msgs), m.notice)
	}
	m = drive(t, m, key("D"), key("y"))
	if len(m.msgs) != 2 || m.notice != "deleted for good" {
		t.Fatalf("after yes: %d messages, notice %q", len(m.msgs), m.notice)
	}
	m = drive(t, m, keys("ga")...)
	if len(m.msgs) != 2 {
		t.Errorf("all mail has %d messages, want 2", len(m.msgs))
	}
	m = drive(t, m, key("z"))
	if m.notice != "nothing to undo" {
		t.Errorf("delete for good must not be undoable, notice %q", m.notice)
	}
}

func TestExpiryLine(t *testing.T) {
	for tags, want := range map[string]string{
		"":                                  "",
		"deleted":                           "In the trash",
		"deleted delete-after:2026-11-14":   "In the trash until 14 Nov 2026 (# restores)",
		"delete-after:2026-10-20":           "Expires on 20 Oct 2026, then deleted for good",
		"delete-after:2026-10-15 something": "Deleted for good at the next sync",
	} {
		if got := expiryLine(strings.Fields(tags), testNow); got != want {
			t.Errorf("expiryLine(%q) = %q, want %q", tags, got, want)
		}
	}
}

func TestSwitchBodyVersion(t *testing.T) {
	m := newTestModel(t, 120, 30)
	export := filepath.Join(t.TempDir(), "export", "Inbox")
	if err := os.MkdirAll(export, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := "From: news@example.org\r\nTo: me@example.org\r\nMessage-ID: <alt@x>\r\nDate: Thu, 15 Oct 2026 09:00:00 +0000\r\n" +
		"Subject: Newsletter\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\n*** plain version ***\r\n" +
		"--b\r\nContent-Type: text/html\r\n\r\n<p>html version</p><p style=\"display:none\">hidden preheader</p>\r\n--b--\r\n"
	if err := os.WriteFile(filepath.Join(export, "n.eml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Call(t.Context(), m.env, ops.Delete, ops.ImportRun,
		ops.ImportIn{Path: filepath.Dir(export), Account: "personal", Inbox: true}); err != nil {
		t.Fatal(err)
	}
	m = drive(t, m, keys("gi")...)
	m = drive(t, m, append(keys("gg"), key("enter"))...)
	if s := screen(m); !strings.Contains(s, "html version") || strings.Contains(s, "hidden preheader") {
		t.Fatalf("default should be the HTML version without hidden text:\n%s", s)
	}
	m = drive(t, m, key("v"))
	if s := screen(m); !strings.Contains(s, "plain version") || m.bodyPrefer != "plain" {
		t.Fatalf("after v:\n%s", s)
	}
	m = drive(t, m, key("v"))
	if !strings.Contains(screen(m), "html version") {
		t.Error("v did not switch back")
	}
	m = drive(t, m, key("J"))
	m = drive(t, m, key("v"))
	if m.notice != "this message has only one version" {
		t.Errorf("notice = %q", m.notice)
	}
}

func TestHelpLegendUsesListColors(t *testing.T) {
	m := newTestModel(t, 120, 40)
	help := strings.Join(m.helpLines(), "\n")
	for _, want := range []string{m.st.server.Render("◐"), m.st.leaving.Render("◕"), m.st.local.Render("●"),
		m.st.unreadDot.Render("◆"), m.st.flag.Render("★"), m.st.tag.Render("#tag"), m.st.accent.Render(pad("e archive", 9))} {
		if !strings.Contains(help, want) || want == ansi.Strip(want) {
			t.Errorf("help lacks styled %q", ansi.Strip(want))
		}
	}
	m.prefs.ASCII = true
	if s := ansi.Strip(strings.Join(m.helpLines(), "\n")); !strings.Contains(s, "   S  on a server too") || strings.Contains(s, "◆") {
		t.Errorf("ASCII legend:\n%s", s)
	}
}

func TestHelpScrolls(t *testing.T) {
	m := newTestModel(t, 80, 24)
	m = drive(t, m, key("?"))
	if !strings.Contains(screen(m), "j/k scroll") || !strings.Contains(screen(m), "Move") {
		t.Fatalf("help top:\n%s", screen(m))
	}
	for range 50 {
		m = drive(t, m, key("j"))
	}
	if s := screen(m); !m.help || !strings.Contains(s, "nothing is ever loaded") || strings.Contains(s, "first, last") {
		t.Fatalf("help bottom:\n%s", s)
	}
	m = drive(t, m, key("x"))
	if m.help || m.helpTop != 0 {
		t.Error("another key must close the help")
	}
	// Wide terminals show the keys in two columns.
	m = newTestModel(t, 120, 60)
	m = drive(t, m, key("?"))
	if !strings.Contains(screen(m), "Move") || !regexp.MustCompile(`Move\s+Act on a message`).MatchString(screen(m)) {
		t.Errorf("two columns:\n%s", screen(m))
	}
}

func TestTrashViewExplainsItself(t *testing.T) {
	m := newTestModel(t, 140, 30)
	m = drive(t, m, key("#"))
	m = drive(t, m, keys("gt")...)
	s := screen(m)
	if !strings.Contains(s, "Trash  1 messages  · deleted for good 30 days after deletion · # restores · :empty-trash") ||
		!strings.Contains(s, "# restore  D delete now") {
		t.Errorf("trash header:\n%s", s)
	}
	// Trashed at testNow: deleted for good on the 30th day.
	if row := ansi.Strip(m.renderListLine(m.msgs[0], false, 60)); !strings.HasSuffix(row, " ⌛30d") {
		t.Errorf("row %q lacks the countdown", row)
	}
	// A long subject never pushes the countdown off the row.
	long := m.msgs[0]
	long.Subject = strings.Repeat("very long subject ", 20)
	if row := ansi.Strip(m.renderListLine(long, false, 50)); !strings.HasSuffix(row, " ⌛30d") || ansi.StringWidth(row) != 50 {
		t.Errorf("long row %q (width %d)", row, ansi.StringWidth(row))
	}
	m.prefs.ASCII = true
	if row := ansi.Strip(m.renderListLine(m.msgs[0], false, 60)); !strings.HasSuffix(row, " 30d") {
		t.Errorf("ASCII row %q", row)
	}
}

func TestCountdown(t *testing.T) {
	m := newTestModel(t, 80, 24)
	for tags, want := range map[string]string{
		"inbox":                           "",
		"deleted delete-after:2026-11-14": "⌛30d",
		"delete-after:2026-10-16":         "⌛1d",
		"delete-after:2026-10-15":         "⌛today",
		"delete-after:2026-10-01":         "⌛today",
	} {
		if got := m.countdown(ops.MessageOut{Tags: strings.Fields(tags)}); got != want {
			t.Errorf("countdown(%q) = %q, want %q", tags, got, want)
		}
	}
}

func TestEmptyTrash(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("#"))
	run := func(m Model, answer string) Model {
		m = drive(t, m, append(append([]tea.Msg{key(":")}, keys("empty-trash")...), key("enter"))...)
		if m.inputKind != inputConfirmEmptyTrash || !strings.Contains(screen(m), "Delete everything in the trash for good") {
			t.Fatalf("no confirmation:\n%s", screen(m))
		}
		return drive(t, m, key(answer))
	}
	m = run(m, "n")
	if m.notice != "trash kept" || !slices.ContainsFunc(m.side, func(s sideItem) bool { return s.label == "Trash" }) {
		t.Fatalf("after no: %q", m.notice)
	}
	m = run(m, "y")
	if m.notice != "deleted 1 for good" || slices.ContainsFunc(m.side, func(s sideItem) bool { return s.label == "Trash" }) {
		t.Fatalf("after yes: %q, sidebar %+v", m.notice, m.side)
	}
	m = drive(t, m, keys("ga")...)
	if len(m.msgs) != 2 {
		t.Errorf("all mail has %d messages, want 2", len(m.msgs))
	}
}

func TestSyncSummaryMentionsOtherDevices(t *testing.T) {
	r := summarize(ops.SyncOut{Accounts: []ops.AccountSyncOut{{ArchivedElsewhere: 2, DeletedElsewhere: 1, TrashCleaned: 1,
		Folders: []ops.FolderSyncOut{{Fetched: 3, Expunged: 1}}}}}, nil)
	if got := r.String(); got != "synced: 3 new, 2 left the server; on another device: 2 archived, 1 deleted" {
		t.Errorf("summary = %q", got)
	}
	if !r.worthTelling() {
		t.Error("changes made on other devices are news")
	}
	held := summarize(ops.SyncOut{Accounts: []ops.AccountSyncOut{{RemovalsHeld: true}}}, nil)
	if !held.worthTelling() || !strings.Contains(held.String(), ":sync accept-removals") {
		t.Errorf("held removal = %q", held.String())
	}
}

func TestSyncAcceptRemovalsCommand(t *testing.T) {
	m := newTestModel(t, 120, 30)
	next, cmd := m.command("sync accept-removals")
	if !next.syncing || !next.syncAccept || cmd == nil {
		t.Fatalf("syncing %v accept %v", next.syncing, next.syncAccept)
	}
	next, _ = next.syncDone(syncDoneMsg{})
	if next.syncAccept {
		t.Error("accept-removals must apply to one sync only")
	}
}

func TestHelpDescribesTheInboxModel(t *testing.T) {
	m := newTestModel(t, 120, 60)
	m.deletionOn = true
	s := ansi.Strip(strings.Join(m.helpLines(), "\n"))
	for _, want := range []string{"it leaves the server at the next sync", "On your phone"} {
		if !strings.Contains(s, want) {
			t.Errorf("help lacks %q:\n%s", want, s)
		}
	}
	m.deletionOn = false
	if s := ansi.Strip(strings.Join(m.helpLines(), "\n")); !strings.Contains(s, "deletion is off: the server copy stays") {
		t.Errorf("help with deletion off:\n%s", s)
	}
}

// Origin folders are listed under a Folders heading, without the folder:
// prefix; INBOX is left out (the Inbox view is the inbox).
func TestSidebarFolders(t *testing.T) {
	m := newTestModel(t, 120, 30)
	if _, err := ops.Call(t.Context(), m.env, ops.Delete, ops.MsgTag,
		ops.TagIn{Query: "subject:invoice", Add: []string{"folder:archive/2024", "bills"}}); err != nil {
		t.Fatal(err)
	}
	m = drive(t, m, m.loadSidebar()())
	var labels []string
	for _, it := range m.side {
		labels = append(labels, it.label)
	}
	if slices.ContainsFunc(labels, func(l string) bool { return strings.Contains(l, "folder:") }) {
		t.Errorf("folder tags listed as tags: %v", labels)
	}
	i := slices.IndexFunc(m.side, func(it sideItem) bool { return it.label == "archive/2024" })
	if i < 0 || m.side[i].query != "tag:folder:archive/2024" || m.side[i].heading != "Folders" {
		t.Fatalf("no archive/2024 folder entry: %+v", m.side)
	}
	if !slices.ContainsFunc(m.side, func(it sideItem) bool { return it.label == "#bills" }) {
		t.Errorf("user tag missing: %v", labels)
	}
	if s := screen(m); !strings.Contains(s, "Folders") || strings.Contains(s, "#folder:") {
		t.Errorf("sidebar:\n%s", s)
	}
}

// Archiving where deletion is off says once per account that the mail
// stays on the server, and only when that is known to be true.
func TestArchiveNotesDeletionOff(t *testing.T) {
	m := newTestModel(t, 120, 30)
	onServer := func(m Model) Model { // the test archive has no server copies: pretend it has
		for i := range m.msgs {
			m.msgs[i].Server = []ops.ServerLocation{{Folder: "INBOX"}}
		}
		return m
	}
	press := func(m Model, status []ops.AccountStatusOut) Model {
		m.status = status
		return drive(t, onServer(m), key("e"))
	}
	off := []ops.AccountStatusOut{{Account: "personal", Deletion: false}}
	if m = press(m, nil); m.notice != "archived" {
		t.Errorf("status unknown: %q (say nothing rather than guess)", m.notice)
	}
	if m = press(m, off); !strings.Contains(m.notice, "deletion is off for personal") {
		t.Fatalf("first archive with deletion off: %q", m.notice)
	}
	if m = press(m, off); m.notice != "archived" {
		t.Errorf("second archive: %q (the note is said once per account)", m.notice)
	}
}

func TestNoDeletionNoteForLocalMail(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m.status = []ops.AccountStatusOut{{Account: "personal", Deletion: false}}
	if m = drive(t, m, key("e")); m.notice != "archived" {
		t.Errorf("local-only mail: %q (nothing is on a server)", m.notice)
	}
}

// The sidebar scrolls to keep the selection visible, headings included.
func TestSidebarScrollsToTheSelection(t *testing.T) {
	m := newTestModel(t, 120, 20)
	for i := range 40 {
		it := sideItem{label: fmt.Sprintf("folder%02d", i), query: fmt.Sprintf("tag:folder:f%02d", i), count: 1}
		if i == 0 {
			it.heading = "Folders"
		}
		m.side = append(m.side, it)
	}
	m.focus = focusSidebar
	for range len(m.side) {
		m = drive(t, m, key("j"))
	}
	if s := screen(m); !strings.Contains(s, "folder39") {
		t.Errorf("the selected last folder is not visible:\n%s", s)
	}
	m = drive(t, m, keys("gg")...) // back in the list; first entries show again
	m.focus, m.sideIdx = focusSidebar, 0
	if s := screen(m); !strings.Contains(s, "Inbox") {
		t.Errorf("top of the sidebar not visible:\n%s", s)
	}
}

// Archive shows what was dealt with: out of the inbox, not in the trash.
func TestArchiveView(t *testing.T) {
	m := newTestModel(t, 120, 30)
	m = drive(t, m, key("e")) // archive the first message
	m = drive(t, m, key("j"), key("#"))
	m = drive(t, m, keys("ge")...)
	if m.query != queryArchive || len(m.msgs) != 1 || m.msgs[0].Subject != "Lunch on Friday?" {
		t.Fatalf("archive view: query %q, %+v", m.query, m.msgs)
	}
	if !slices.ContainsFunc(m.side, func(it sideItem) bool { return it.label == "Archive" && it.query == queryArchive }) {
		t.Errorf("no Archive in the sidebar: %+v", m.side)
	}
	if s := screen(m); !strings.Contains(s, "nought  Archive  1 messages") {
		t.Errorf("header:\n%s", s)
	}
}

// Each quote level has its own color, so a level can be followed.
func TestQuoteLevelsHaveTheirOwnColors(t *testing.T) {
	m := newTestModel(t, 120, 30)
	for _, o := range []ops.ShowOut{
		{BodyFrom: "text/plain", Source: "Thanks!\n> Sure.\n> > Lunch?\n"},
		{BodyFrom: "text/html", Source: "Thanks!<blockquote>Sure.<blockquote>Lunch?</blockquote></blockquote>"},
	} {
		body, _ := m.styledBody(&o)
		one, two := m.st.quote[0].Render("│ "), m.st.quote[1].Render("│ ")
		if m.st.quote[1].GetForeground() != lipgloss.Color("5") {
			t.Error("level 2 is not magenta: yellow is for warnings")
		}
		if one == two || !strings.Contains(body, one+two) || !strings.Contains(ansi.Strip(body), "│ │ Lunch?") {
			t.Errorf("%s: levels not told apart: %q", o.BodyFrom, body)
		}
	}
}

// A quoted line that wraps keeps its bars on every line, so the nesting
// stays visible in a narrow pane.
func TestWrappedQuotesKeepTheirBars(t *testing.T) {
	m := newTestModel(t, 120, 30)
	bar := m.st.quote[0].Render("│ ")
	got := wrapBody("plain words that wrap\n"+bar+m.st.quote[1].Render("│ ")+"one two three four", 12)
	want := "plain words\nthat wrap\n│ │ one two\n│ │ three\n│ │ four"
	if ansi.Strip(got) != want || !strings.HasPrefix(strings.Split(got, "\n")[3], bar) {
		t.Errorf("got:\n%s\nwant:\n%s", ansi.Strip(got), want)
	}
}

// "replied" is state (is:replied), like unread and flagged: not a tag of
// the user's in the sidebar.
func TestSidebarHidesReplied(t *testing.T) {
	m := newTestModel(t, 120, 30)
	if _, err := ops.Call(t.Context(), m.env, ops.Delete, ops.MsgTag, ops.TagIn{Query: "subject:lunch", Add: []string{ops.TagReplied}}); err != nil {
		t.Fatal(err)
	}
	m = drive(t, m, sidebarDoneMsg{items: run(m.loadSidebar())[0].(sidebarDoneMsg).items})
	if strings.Contains(screen(m), "#replied") {
		t.Errorf("sidebar:\n%s", screen(m))
	}
}

// Opening a link crashed: nothing set the default opener outside tests.
func TestLinksOpenWithTheSystemOpener(t *testing.T) {
	m := newModel(t.Context(), newTestModel(t, 120, 30).env)
	if m.openURL == nil {
		t.Fatal("no default link opener")
	}
	u := "https://example.org/a?b=1&c=2"
	for goos, want := range map[string][]string{
		"linux":   {"xdg-open", u},
		"freebsd": {"xdg-open", u},
		"darwin":  {"open", u},
		// Not "cmd /c start": cmd would read the & in the URL as a command.
		"windows": {"rundll32", "url.dll,FileProtocolHandler", u},
	} {
		if got := browserCommand(goos, u); !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", goos, got, want)
		}
	}
}
