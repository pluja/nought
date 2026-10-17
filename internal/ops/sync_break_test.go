// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"fmt"
	"slices"
	"testing"

	"github.com/pluja/nought/internal/imaptest"
)

// breakScenario is an account with something for every sync phase to do:
// new mail to fetch, archived mail due for deletion, a folder kept zero
// days, and a message another client moved to the trash.
func breakScenario(t *testing.T) (*Env, *imaptest.Server) {
	t.Helper()
	env, srv := syncEnv(t, imaptest.Options{}, true)
	srv.Append(t, "INBOX", mail("old@x", "old"), daysAgo(env, 30))
	srv.Append(t, "INBOX", mail("keep@x", "keep"), daysAgo(env, 0))
	elsewhere := srv.Append(t, "INBOX", mail("elsewhere@x", "elsewhere"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	archiveMail(t, env, "subject:old")
	srv.Move(t, "INBOX", elsewhere, "Trash")
	srv.Append(t, "INBOX", mail("new@x", "new"), daysAgo(env, 0))
	srv.Append(t, "Sent", mail("sent@x", "sentone"), daysAgo(env, 0))
	return env, srv
}

// syncState is what a sync leaves, to compare runs.
func syncState(t *testing.T, env *Env, srv *imaptest.Server) string {
	t.Helper()
	state := ""
	for _, f := range []string{"INBOX", "Sent", "Trash"} {
		state += fmt.Sprintf("%s:%d ", f, len(srv.UIDs(t, f)))
	}
	for _, q := range []string{"", "tag:inbox", "tag:deleted", "tag:folder:sent"} {
		got := search(t, env, q)
		slices.Sort(got)
		state += fmt.Sprintf("[%s] %v ", q, got)
	}
	return state
}

// A connection that drops at any point of a sync loses no mail, deletes
// nothing without a journal entry, and leaves nothing the next syncs do not
// finish: they end where unbroken syncs end.
func TestSyncSurvivesADropAnywhere(t *testing.T) {
	if testing.Short() {
		t.Skip("one sync per break point")
	}
	env, srv := breakScenario(t)
	for range 3 {
		runSyncOK(t, env, SyncIn{})
	}
	want := syncState(t, env, srv)

	const step = 64
	for n := int64(step); ; n += step {
		env, srv := breakScenario(t)
		srv.BreakNextConnection(n)
		_, err := Call(t.Context(), env, Delete, SyncRun, SyncIn{})
		// Mail gone from the server is in the archive, and was journaled.
		if len(srv.UIDs(t, "Sent")) == 0 && !slices.Contains(search(t, env, ""), "sentone") {
			t.Fatalf("break at %d: the sent message is neither on the server nor archived", n)
		}
		gone := 5 // INBOX: old, keep, new; Sent: sentone; Trash: elsewhere
		for _, f := range []string{"INBOX", "Sent", "Trash"} {
			gone -= len(srv.UIDs(t, f))
		}
		if gone > len(readJournal(t, env)) {
			t.Fatalf("break at %d: %d messages deleted, %d journaled", n, gone, len(readJournal(t, env)))
		}
		for range 2 {
			runSyncOK(t, env, SyncIn{})
		}
		if got := syncState(t, env, srv); got != want {
			t.Fatalf("break at %d:\n got %s\nwant %s", n, got, want)
		}
		if err == nil { // the sync finished before the break
			t.Logf("%d break points", n/step)
			return
		}
	}
}
