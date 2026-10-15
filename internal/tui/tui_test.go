// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"nought/internal/config"
	"nought/internal/ops"
	"nought/internal/secret"
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
	case <-time.After(50 * time.Millisecond):
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
	for _, want := range []string{"tag:inbox", "3 messages", "Lunch on Friday?", "Invoice 2026-114",
		"Inbox", "no accounts", "? help"} {
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
	if !hasTag(m.msgs[0], ops.TagUnread) {
		t.Fatal("expected an unread message")
	}
	m = drive(t, m, key("enter"))
	if m.focus != focusReader || m.open == nil || m.open.Message.Subject != "Lunch on Friday?" {
		t.Fatalf("focus %v, open %+v", m.focus, m.open)
	}
	if hasTag(m.msgs[0], ops.TagUnread) {
		t.Error("opening did not mark the message read")
	}
	s := screen(m)
	for _, want := range []string{"Pizza at noon?", "Local only: no longer on any server"} {
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
	if len(m.msgs) != 3 || !strings.Contains(m.notice, "undid") {
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
	if !hasTag(m.msgs[0], ops.TagFlagged) {
		t.Fatal("not flagged")
	}
	m = drive(t, m, append([]tea.Msg{key("l")}, append(keys("+todo -flagged"), key("enter"))...)...)
	if !hasTag(m.msgs[0], "todo") || hasTag(m.msgs[0], ops.TagFlagged) {
		t.Errorf("tags = %v", m.msgs[0].Tags)
	}
	m = drive(t, m, key("u"), key("u"))
	if !hasTag(m.msgs[0], ops.TagUnread) {
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
	if !strings.Contains(screen(m), "Nothing in this") || !m.help {
		t.Error("help not shown")
	}
	m = drive(t, m, key("x"))
	if m.help {
		t.Error("help not closed")
	}
	m = drive(t, m, key("r"))
	if !strings.Contains(m.notice, "sub-project 4") {
		t.Errorf("reply notice = %q", m.notice)
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
		{nil, "Local only", "  "},
		{[]ops.ServerLocation{{Folder: "INBOX"}}, "stays there (deletion is off", "☁ "},
		{[]ops.ServerLocation{{Folder: "INBOX", LeavesAt: &soon}}, "leaves the server in 3 days", "⏳"},
		{[]ops.ServerLocation{{Folder: "INBOX", LeavesAt: &past}}, "at the next sync", "⏳"},
	}
	m := Model{now: func() time.Time { return testNow }}
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
	if got := m.marker(ops.MessageOut{Server: []ops.ServerLocation{{LeavesAt: &soon}}}); got != "S>" {
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
