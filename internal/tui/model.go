// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tui is nought's terminal interface: a keyboard-first view for
// Inbox Zero triage. Like the CLI, it is a frontend over internal/ops and
// contains no mail logic of its own. It always shows where a message lives:
// still on a server, leaving soon, or only in the local archive.
package tui

import (
	"context"
	"image/color"
	"io"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"nought/internal/config"
	"nought/internal/ops"
	"nought/internal/render"
)

// Permissions: the TUI acts for the local user. Everyday actions use the
// tag level; sync, the trash and deleting for good (all of which delete
// mail, some later) use the highest.
const (
	permTag    = ops.Tag
	permDelete = ops.Delete
)

// Default views.
const (
	queryInbox   = "tag:inbox"
	queryUnread  = "is:unread"
	queryFlagged = "is:flagged"
	queryAll     = ""
	queryTrash   = "tag:deleted"
)

type focus int

const (
	focusList focus = iota
	focusReader
	focusSidebar
)

type inputKind int

const (
	inputNone inputKind = iota
	inputSearch
	inputTags
	inputPalette
	inputConfirmSync
	inputLinkNumber
	inputConfirmDelete
)

// sideItem is one entry in the sidebar.
type sideItem struct {
	label string
	query string
	count int // -1 when not counted
}

// undoEntry reverts one tag change.
type undoEntry struct {
	query  string
	add    []string
	remove []string
	what   string
}

// tagReq is a queued tag change. Changes run one at a time, in the order
// the keys were pressed.
type tagReq struct {
	msg    ops.MessageOut
	add    []string
	remove []string
	what   string // "" for changes that are not undoable (mark read on open)
	undo   *undoEntry
}

// styles use the terminal's own 16-color palette, so they follow the
// user's theme (light or dark) instead of imposing one.
type styles struct {
	plain, title, accent, dim, bold, warn, errText lipgloss.Style
	unreadDot, flag, tag, italic, link             lipgloss.Style
	local, server, leaving                         lipgloss.Style // where a message lives
	selBG                                          color.Color
}

func newStyles() styles {
	c := lipgloss.Color
	s := lipgloss.NewStyle()
	return styles{
		plain:     s,
		title:     s.Bold(true).Foreground(c("6")),
		accent:    s.Foreground(c("6")),
		dim:       s.Faint(true),
		bold:      s.Bold(true),
		warn:      s.Foreground(c("3")),
		errText:   s.Foreground(c("1")).Bold(true),
		unreadDot: s.Foreground(c("4")).Bold(true),
		flag:      s.Foreground(c("3")).Bold(true),
		tag:       s.Foreground(c("5")),
		italic:    s.Italic(true),
		link:      s.Foreground(c("4")).Underline(true),
		local:     s.Foreground(c("2")),
		server:    s.Foreground(c("4")),
		leaving:   s.Foreground(c("3")),
		selBG:     c("8"),
	}
}

// Model is the whole TUI state. Bubble Tea passes it by value.
//
// It keeps the program's context: a Bubble Tea model lives as long as the
// program, and its commands must stop when the program is cancelled. This
// is the one deliberate exception to "never store a context".
type Model struct {
	ctx  context.Context
	env  *ops.Env
	send func(tea.Msg) // delivers progress from background work
	now  func() time.Time
	st   styles

	width, height int
	prefs         config.TUI

	query     string
	searchGen int // results of older searches are ignored
	msgs      []ops.MessageOut
	more      bool
	cursor    int
	offset    int

	focus   focus
	sidebar bool
	side    []sideItem
	sideIdx int

	open        *ops.ShowOut
	openKey     string // message, width and tags the reader content was built for
	links       []render.Link
	showLinks   bool
	linkPick    int // >0: the confirmation box for this link is open
	openURL     func(string) error
	showGen     int    // results of older show requests are ignored
	bodyPrefer  string // "html" or "plain" once the user switched with v; "" follows the config
	previewSeq  int    // debounces previews while moving
	previewWait time.Duration
	reader      viewport.Model
	inputKind   inputKind
	input       textinput.Model
	tagQueue    []tagReq
	tagBusy     bool
	status      []ops.AccountStatusOut
	notice      string
	noticeErr   bool           // the notice reports a failure
	deleting    ops.MessageOut // the message D asks about
	syncing     bool
	syncCancel  context.CancelFunc
	quitting    bool
	syncLines   []string
	help        bool
	pendingG    bool
	undo        []undoEntry
}

// Options configure Run.
type Options struct {
	Input  io.Reader
	Output io.Writer
	// OpenURL opens a link in the user's browser; nil uses the system's
	// default handler.
	OpenURL func(string) error
}

// Run starts the TUI and returns when the user quits.
func Run(ctx context.Context, env *ops.Env, opts Options) error {
	m := newModel(ctx, env)
	progOpts := []tea.ProgramOption{tea.WithContext(ctx)}
	if opts.Input != nil {
		progOpts = append(progOpts, tea.WithInput(opts.Input))
	}
	if opts.Output != nil {
		progOpts = append(progOpts, tea.WithOutput(opts.Output))
	}
	if opts.OpenURL != nil {
		m.openURL = opts.OpenURL
	}
	sender := &sendBox{}
	m.send = sender.send
	p := tea.NewProgram(m, progOpts...)
	sender.p = p
	_, err := p.Run()
	if err != nil && ctx.Err() != nil {
		return nil // interrupted: a normal way to leave
	}
	return err
}

// sendBox lets background work reach the program after it is created.
type sendBox struct{ p *tea.Program }

func (s *sendBox) send(msg tea.Msg) {
	if s.p != nil {
		s.p.Send(msg)
	}
}

func newModel(ctx context.Context, env *ops.Env) Model {
	in := textinput.New()
	in.Prompt = ""
	m := Model{
		ctx:         ctx,
		env:         env,
		send:        func(tea.Msg) {},
		now:         env.Now,
		st:          newStyles(),
		previewWait: previewDelay,
		query:       queryInbox,
		sidebar:     true,
		input:       in,
		reader:      viewport.New(),
	}
	if cfg, err := env.Config(); err == nil {
		m.prefs = cfg.TUI
	}
	return m
}

// Init loads the first view.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.search(m.query, m.searchGen), m.loadStatus(), m.loadSidebar())
}
