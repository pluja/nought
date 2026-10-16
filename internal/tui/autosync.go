// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/pluja/nought/internal/ops"
)

// The TUI syncs on start and then every tui.sync_every (default 10m), so
// the server stays a drop box while the archive keeps up. Sync deletes
// server mail only where the user enabled deletion, or for mail they
// deleted, and always after the local copy is verified; so it runs
// without asking.

// maxSyncBackoff caps the wait after repeated failures (e.g. offline).
const maxSyncBackoff = time.Hour

// autoSyncMsg asks for a sync; one from an older schedule is ignored.
type autoSyncMsg struct{ gen int }

// syncResult condenses a sync.
type syncResult struct {
	fetched, expunged, problems int
	archived, deleted, back     int  // done on another device, mirrored here
	held                        bool // a mass removal waits for :sync accept-removals
	busy                        bool // another sync (e.g. a cron job) was running
}

func summarize(out ops.SyncOut, err error) syncResult {
	var r syncResult
	for _, a := range out.Accounts {
		if a.Error != "" {
			r.problems++
		}
		r.archived += a.ArchivedElsewhere
		r.deleted += a.DeletedElsewhere
		r.back += a.BackInInbox
		r.expunged += a.TrashCleaned
		r.held = r.held || a.RemovalsHeld
		for _, f := range a.Folders {
			r.fetched += f.Fetched
			r.expunged += f.Expunged
			if f.Error != "" {
				r.problems++
			}
		}
	}
	switch {
	case errors.Is(err, ops.ErrBusy):
		r.busy = true
	case err != nil:
		r.problems = max(r.problems, 1)
	}
	return r
}

func (r syncResult) String() string {
	if r.busy {
		return "another sync is running; trying again later"
	}
	s := "synced: " + strconv.Itoa(r.fetched) + " new, " + strconv.Itoa(r.expunged) + " left the server"
	if other := ops.DescribeOtherDevices(r.archived, r.deleted, r.back); other != "" {
		s += "; on another device: " + other
	}
	if r.held {
		s += "; many messages disappeared at once and were kept: if that was you, run :sync accept-removals"
	}
	if r.problems > 0 {
		s += fmt.Sprintf("; %d problems (run 'nou sync' for details)", r.problems)
	}
	return s
}

// worthTelling: automatic syncs stay quiet unless something happened.
func (r syncResult) worthTelling() bool {
	return r.fetched+r.expunged+r.problems+r.archived+r.deleted+r.back > 0 || r.held
}

// nextSyncDelay doubles the interval for each failure in a row, up to an
// hour, so an offline laptop does not retry every few minutes.
func nextSyncDelay(every time.Duration, failures int) time.Duration {
	d := every
	for range failures {
		if d >= maxSyncBackoff {
			break
		}
		d *= 2
	}
	return min(d, max(every, maxSyncBackoff))
}

// scheduleSync starts a new schedule; pending ticks of the old one are
// ignored when they arrive.
func (m Model) scheduleSync() (Model, tea.Cmd) {
	m.syncSchedule++
	if m.syncEvery <= 0 {
		return m, nil
	}
	gen := m.syncSchedule
	return m, tea.Tick(nextSyncDelay(m.syncEvery, m.syncFailures), func(time.Time) tea.Msg { return autoSyncMsg{gen: gen} })
}

func (m Model) autoSync(msg autoSyncMsg) (Model, tea.Cmd) {
	if msg.gen != m.syncSchedule || m.syncing || m.quitting {
		return m, nil // superseded, or a sync is already running (its end reschedules)
	}
	return m.startSync(true)
}

func (m Model) syncDone(msg syncDoneMsg) (Model, tea.Cmd) {
	auto := m.syncAuto
	m.syncing, m.syncLines, m.syncAuto, m.syncAccept = false, nil, false, false
	if m.syncCancel != nil {
		m.syncCancel()
		m.syncCancel = nil
	}
	if m.quitting {
		return m, tea.Quit
	}
	r := summarize(msg.out, msg.err)
	switch {
	case r.busy:
	case r.problems > 0:
		m.syncFailures++
	default:
		m.syncFailures = 0
	}
	if !auto || r.worthTelling() {
		m.notice, m.noticeErr = r.String(), r.problems > 0 || r.held
	}
	m, schedule := m.scheduleSync()
	m, refresh := m.refresh()
	return m, tea.Batch(schedule, refresh)
}
