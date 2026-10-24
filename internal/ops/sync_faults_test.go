// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"bytes"
	"regexp"
	"slices"
	"strconv"
	"testing"

	"github.com/pluja/nought/internal/imaptest"
)

// rewriteNumbers adds delta to the number after each match of prefix in
// what the server writes.
func rewriteNumbers(srv *imaptest.Server, pattern string, delta int) {
	re := regexp.MustCompile(pattern)
	srv.RewriteResponses(func(p []byte) []byte {
		return re.ReplaceAllFunc(p, func(m []byte) []byte {
			sub := re.FindSubmatchIndex(m)
			n, _ := strconv.Atoi(string(m[sub[2]:sub[3]]))
			return slices.Concat(m[:sub[2]], []byte(strconv.Itoa(n+delta)), m[sub[3]:])
		})
	})
}

// A message whose size differs from what the server reports is archived,
// but its server copy is never deleted: it may not be the same message.
func TestMisreportedSizeIsNeverDeleted(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	srv.Append(t, "INBOX", mail("old@x", "old"), daysAgo(env, 30))
	rewriteNumbers(srv, `RFC822\.SIZE (\d+)`, 1)
	out := runSyncOK(t, env, SyncIn{})
	if !hasNote(out, "kept on the server") || len(search(t, env, "subject:old")) != 1 {
		t.Fatalf("sync = %+v", out)
	}
	archiveMail(t, env, "subject:old")
	runSyncOK(t, env, SyncIn{})
	srv.RewriteResponses(nil)
	runSyncOK(t, env, SyncIn{})
	if got := srv.UIDs(t, "INBOX"); len(got) != 1 {
		t.Errorf("deleted a message whose size did not match: %v", got)
	}
}

// A message list shorter than the server's count is not trusted to say
// what other devices removed: the changes wait for a list that adds up.
func TestIncompleteListAppliesNothing(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	srv.Append(t, "INBOX", mail("a@x", "stays"), daysAgo(env, 0))
	gone := srv.Append(t, "INBOX", mail("b@x", "goes"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.Expunge(t, "INBOX", gone)
	rewriteNumbers(srv, `\* (\d+) EXISTS`, 1)
	out := runSyncOK(t, env, SyncIn{})
	if !hasNote(out, "did not match its message count") {
		t.Errorf("no note: %+v", out)
	}
	if got := search(t, env, "tag:inbox"); len(got) != 2 {
		t.Errorf("applied removals from an incomplete list: inbox %v", got)
	}
	srv.RewriteResponses(nil)
	runSyncOK(t, env, SyncIn{})
	if got := search(t, env, "tag:inbox"); !slices.Equal(got, []string{"stays"}) {
		t.Errorf("inbox once the list adds up = %v", got)
	}
}

// emptyBodies makes the server send as empty the bodies that contain
// marker.
func emptyBodies(srv *imaptest.Server, marker string) {
	re := regexp.MustCompile(`BODY\[\] \{(\d+)\}\r\n`)
	srv.RewriteResponses(func(p []byte) []byte {
		var out []byte
		for {
			loc := re.FindSubmatchIndex(p)
			if loc == nil {
				return append(out, p...)
			}
			n, _ := strconv.Atoi(string(p[loc[2]:loc[3]]))
			end := min(loc[1]+n, len(p))
			if !bytes.Contains(p[loc[1]:end], []byte(marker)) {
				out = append(out, p[:end]...)
			} else {
				out = append(append(out, p[:loc[0]]...), "BODY[] {0}\r\n"...)
			}
			p = p[end:]
		}
	})
}

// Review: a message the server would not send (an empty body here; NO or
// no data alike) either stopped its folder at every sync, holding up all
// the mail after it, or was skipped for good. It is remembered like an
// oversized one: the rest is archived, the note comes back at every sync,
// and it is archived once the server sends it.
func TestUnsentMessageDoesNotHoldUpTheFolder(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	srv.Append(t, "INBOX", mail("bad@x", "bad one"), daysAgo(env, 30))
	srv.Append(t, "INBOX", mail("good@x", "good one"), daysAgo(env, 0))
	emptyBodies(srv, "bad one")
	for range 2 {
		out := runSyncOK(t, env, SyncIn{})
		if n := countNotes(folder(t, out, "INBOX"), "did not send it"); n != 1 {
			t.Fatalf("%d notes: %+v", n, out)
		}
		if got := search(t, env, ""); !slices.Equal(got, []string{"good one"}) {
			t.Fatalf("archive %v", got)
		}
	}
	srv.RewriteResponses(nil)
	out := runSyncOK(t, env, SyncIn{})
	if got := search(t, env, ""); len(got) != 2 || hasNote(out, "did not send it") {
		t.Errorf("once the server sends it: archive %v, %+v", got, out)
	}
}
