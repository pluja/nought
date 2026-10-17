// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"slices"
	"strings"
	"testing"

	"github.com/pluja/nought/internal/imaptest"
)

func countNotes(f FolderSyncOut, substr string) int {
	n := 0
	for _, note := range f.Notes {
		if strings.Contains(note, substr) {
			n++
		}
	}
	return n
}

func setMaxMessageMB(t *testing.T, env *Env, mb int) {
	t.Helper()
	cfg, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	next := *cfg
	next.MaxMessageMB = &mb
	if err := saveConfig(env, &next); err != nil {
		t.Fatal(err)
	}
}

// A message over the size limit stays on the server, and every sync says
// so while it is there; raising the limit archives it.
func TestTooLargeMessageIsReportedUntilItFits(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	setMaxMessageMB(t, env, 1)
	srv.Append(t, "INBOX", mail("big@x", "big")+strings.Repeat("x", 3<<19), daysAgo(env, 0))
	srv.Append(t, "INBOX", mail("small@x", "small"), daysAgo(env, 0))
	for range 2 {
		out := runSyncOK(t, env, SyncIn{})
		if n := countNotes(folder(t, out, "INBOX"), "larger than max_message_mb (1)"); n != 1 {
			t.Fatalf("%d size notes: %+v", n, out)
		}
		if got := search(t, env, ""); !slices.Equal(got, []string{"small"}) {
			t.Fatalf("archive = %v", got)
		}
	}
	setMaxMessageMB(t, env, 2)
	if out := runSyncOK(t, env, SyncIn{}); hasNote(out, "max_message_mb") || folder(t, out, "INBOX").Fetched != 1 {
		t.Errorf("sync after raising the limit = %+v", out)
	}
	if got := search(t, env, "tag:inbox"); len(got) != 2 {
		t.Errorf("inbox = %v", got)
	}
	if out := runSyncOK(t, env, SyncIn{}); hasNote(out, "max_message_mb") {
		t.Errorf("note after archiving: %+v", out)
	}
}

// Once the message is gone from the server, the note goes too.
func TestTooLargeMessageGoneFromTheServer(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	setMaxMessageMB(t, env, 1)
	uid := srv.Append(t, "INBOX", mail("big@x", "big")+strings.Repeat("x", 3<<19), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.Expunge(t, "INBOX", uid)
	if out := runSyncOK(t, env, SyncIn{}); hasNote(out, "max_message_mb") {
		t.Errorf("note for a message no longer on the server: %+v", out)
	}
}

// A server that understates a message's size is caught while fetching: the
// message is left on the server, as if its size had been told.
func TestUnderstatedSizeIsCaughtWhileFetching(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	setMaxMessageMB(t, env, 1)
	srv.Append(t, "INBOX", mail("big@x", "big")+strings.Repeat("x", 3<<19), daysAgo(env, 30))
	rewriteNumbers(srv, `RFC822\.SIZE (\d+)`, -(1 << 20))
	for range 2 {
		out := runSyncOK(t, env, SyncIn{})
		if countNotes(folder(t, out, "INBOX"), "larger than max_message_mb") != 1 || len(search(t, env, "")) != 0 {
			t.Fatalf("sync = %+v, archive %v", out, search(t, env, ""))
		}
	}
}
