// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tui is nought's terminal interface: a keyboard-first view for
// Inbox Zero triage. Like the CLI, it is a frontend over internal/ops and
// contains no mail logic of its own. It always shows where a message lives:
// still on a server, leaving soon, or only in the local archive.
package tui

import (
	"context"
	"io"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"nought/internal/config"
	"nought/internal/ops"
)

// The TUI acts for the local user, who holds every permission.
const perm = ops.Delete

// Default views.
const (
	queryInbox   = "tag:inbox"
	queryUnread  = "is:unread"
	queryFlagged = "is:flagged"
	queryAll     = ""
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

// Model is the whole TUI state. Bubble Tea passes it by value.
type Model struct {
	ctx  context.Context
	env  *ops.Env
	send func(tea.Msg) // delivers progress from background work
	now  func() time.Time

	width, height int
	prefs         config.TUI

	query  string
	msgs   []ops.MessageOut
	more   bool
	cursor int
	offset int

	focus   focus
	sidebar bool
	side    []sideItem
	sideIdx int

	open   *ops.ShowOut
	reader viewport.Model

	input     textinput.Model
	inputKind inputKind

	status    []ops.AccountStatusOut
	notice    string
	syncing   bool
	syncLines []string
	help      bool
	pendingG  bool
	undo      []undoEntry
}

// Options configure Run.
type Options struct {
	Input  io.Reader
	Output io.Writer
}

// Run starts the TUI and returns when the user quits.
func Run(ctx context.Context, env *ops.Env, opts Options) error {
	m := newModel(ctx, env)
	var progOpts []tea.ProgramOption
	progOpts = append(progOpts, tea.WithContext(ctx))
	if opts.Input != nil {
		progOpts = append(progOpts, tea.WithInput(opts.Input))
	}
	if opts.Output != nil {
		progOpts = append(progOpts, tea.WithOutput(opts.Output))
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
		ctx:     ctx,
		env:     env,
		send:    func(tea.Msg) {},
		now:     env.Now,
		query:   queryInbox,
		sidebar: true,
		input:   in,
		reader:  viewport.New(),
	}
	if cfg, err := env.Config(); err == nil {
		m.prefs = cfg.TUI
	}
	return m
}

// Init loads the first view.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.search(m.query), m.loadStatus(), m.loadSidebar())
}
