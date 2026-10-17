// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/pluja/nought/internal/config"
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

// A rule with the query * tags every new message (the "classify" pattern
// for scripts); actions take * as an explicit "everything", never "".
func TestStarMatchesEverything(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	cfg, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	next := *cfg
	next.Rules = []config.Rule{{Query: "*", Tags: []string{"classify"}}}
	if err := saveConfig(env, &next); err != nil {
		t.Fatal(err)
	}
	srv.Append(t, "INBOX", mail("a@x", "one"), daysAgo(env, 0))
	srv.Append(t, "INBOX", mail("b@x", "two"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	if got := search(t, env, "tag:classify"); len(got) != 2 {
		t.Errorf("tagged by the * rule: %v", got)
	}
	if _, err := Call(t.Context(), env, Tag, MsgArchive, ArchiveIn{Query: ""}); !errors.Is(err, ErrUsage) {
		t.Errorf("archive with an empty query = %v", err)
	}
	if out, err := Call(t.Context(), env, Tag, MsgArchive, ArchiveIn{Query: "*"}); err != nil || out.Changed != 2 {
		t.Errorf("archive * = %+v, %v", out, err)
	}
}
