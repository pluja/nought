// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
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

// emptyBodies makes the server send every message body as empty.
func emptyBodies(srv *imaptest.Server) {
	re := regexp.MustCompile(`BODY\[\] \{(\d+)\}\r\n`)
	srv.RewriteResponses(func(p []byte) []byte {
		var out []byte
		for {
			loc := re.FindSubmatchIndex(p)
			if loc == nil {
				return append(out, p...)
			}
			n, _ := strconv.Atoi(string(p[loc[2]:loc[3]]))
			out = append(append(out, p[:loc[0]]...), "BODY[] {0}\r\n"...)
			p = p[min(loc[1]+n, len(p)):]
		}
	})
}

// An empty body from the server is a failed fetch, tried again by the next
// sync: skipped, the message would never be archived (the folder's cursor
// moves past it), though it stays on the server.
func TestEmptyMessageIsFetchedAgain(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	srv.Append(t, "INBOX", mail("old@x", "old"), daysAgo(env, 30))
	emptyBodies(srv)
	out, err := Call(t.Context(), env, Delete, SyncRun, SyncIn{})
	if !errors.Is(err, ErrPartial) || !strings.Contains(folder(t, out, "INBOX").Error, "empty message") || len(search(t, env, "")) != 0 {
		t.Fatalf("sync = %+v, %v, archive %v", out, err, search(t, env, ""))
	}
	srv.RewriteResponses(nil)
	runSyncOK(t, env, SyncIn{})
	if got := search(t, env, ""); !slices.Equal(got, []string{"old"}) {
		t.Errorf("once the server sends it, the archive has %v", got)
	}
}
