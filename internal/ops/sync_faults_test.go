// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
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
