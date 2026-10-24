// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/imaptest"
	"github.com/pluja/nought/internal/query"
	"github.com/pluja/nought/internal/store"
)

func files(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir + "/cur")
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestTrashAndRestore(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	id := idOf(t, env, "subject:\"read one\"")

	out, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: id})
	if err != nil || out.Matched != 1 || out.Purged != 0 {
		t.Fatalf("delete = %+v, %v", out, err)
	}
	visible, err := Call(t.Context(), env, Read, MsgSearch, SearchIn{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range visible.Messages {
		if m.Subject == "read one" {
			t.Error("deleted mail still in searches")
		}
	}
	if n, _ := Call(t.Context(), env, Read, MsgCount, CountIn{}); n.Count != 2 {
		t.Errorf("count includes deleted mail: %d", n.Count)
	}
	if got := search(t, env, "tag:deleted"); !slices.Equal(got, []string{"read one"}) {
		t.Errorf("trash = %v", got)
	}
	if files(t, env.Paths.AccountArchive("personal")) != 2 || files(t, env.Paths.TransientArchive("personal")) != 1 {
		t.Error("file not moved out of the backed-up archive")
	}
	dump, _ := os.ReadFile(env.Paths.TagsDump)
	transient, _ := os.ReadFile(env.Paths.TransientDump)
	if strings.Contains(string(dump), "1@x") || !strings.Contains(string(transient), "1@x") {
		t.Errorf("dumps:\n%s\n---\n%s", dump, transient)
	}

	// Reindex keeps the trash (from the transient dump).
	if _, err := Call(t.Context(), env, Delete, IndexRebuild, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if got := search(t, env, "tag:deleted"); !slices.Equal(got, []string{"read one"}) {
		t.Errorf("trash after reindex = %v", got)
	}

	res, err := Call(t.Context(), env, Delete, MsgRestore, RestoreIn{Query: "tag:deleted"})
	if err != nil || res.Changed != 1 {
		t.Fatalf("restore = %+v, %v", res, err)
	}
	if got := search(t, env, "tag:inbox"); !slices.Contains(got, "read one") {
		t.Errorf("not back in the inbox: %v", got)
	}
	if files(t, env.Paths.TransientArchive("personal")) != 0 {
		t.Error("file not moved back")
	}
}

func TestDeleteNowAndTrashExpiry(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	out, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: idOf(t, env, "subject:\"read one\""), Now: true})
	if err != nil || out.Purged != 1 || out.Pending != 0 {
		t.Fatalf("delete now = %+v, %v", out, err)
	}
	if got := search(t, env, "tag:deleted"); len(got) != 0 {
		t.Errorf("still in the trash: %v", got)
	}
	if files(t, env.Paths.TransientArchive("personal"))+files(t, env.Paths.AccountArchive("personal")) != 2 {
		t.Error("file not removed")
	}

	// Trashed mail is purged once trash_days have passed.
	if _, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: idOf(t, env, "subject:\"sent three\"")}); err != nil {
		t.Fatal(err)
	}
	if n, _ := purge(t.Context(), env); len(n) != 0 {
		t.Fatal("purged before trash_days")
	}
	later := env.Now().AddDate(0, 0, config.DefaultTrashDays)
	env.Now = func() time.Time { return later }
	if n, err := purge(t.Context(), env); err != nil || len(n) != 1 {
		t.Errorf("purge after trash_days = %d, %v", len(n), err)
	}

	empty, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: idOf(t, env, "subject:\"unread two\"")})
	if err != nil || empty.Matched != 1 {
		t.Fatal(err)
	}
	te, err := Call(t.Context(), env, Delete, TrashEmpty, TrashEmptyIn{})
	if err != nil || te.Purged != 1 {
		t.Errorf("trash empty = %+v, %v", te, err)
	}
	if _, err := Call(t.Context(), env, Tag, MsgDelete, DeleteIn{Query: "x"}); !errors.Is(err, ErrPermission) {
		t.Errorf("tag-level delete = %v", err)
	}
}

func TestDeleteRemovesServerCopyEvenWithDeletionOff(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	srv.Append(t, "INBOX", mail("code@x", "your code is 123456"), daysAgo(env, 0))
	srv.Append(t, "INBOX", mail("keep@x", "keep me"), daysAgo(env, 30))
	runSyncOK(t, env, SyncIn{})

	out, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: idOf(t, env, "subject:code"), Now: true})
	if err != nil || out.Purged != 0 || out.Pending != 1 {
		t.Fatalf("delete now with a server copy = %+v, %v", out, err)
	}
	runSyncOK(t, env, SyncIn{})
	if got := srv.UIDs(t, "INBOX"); len(got) != 1 {
		t.Fatalf("server = %v (only the deleted message should be gone)", got)
	}
	if got := search(t, env, "tag:deleted OR subject:code"); len(got) != 0 {
		t.Errorf("not purged after the server copy was removed: %v", got)
	}
	for _, e := range readJournal(t, env) {
		if e.MessageID != "" || e.Hash != "" {
			t.Errorf("journal identifies deleted mail: %+v", e)
		}
	}
}

func TestExpiringRule(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	cfg, _ := env.Config()
	next := &config.Config{Accounts: cfg.Accounts, Rules: []config.Rule{
		{Query: `subject:"verification code" OR subject:otp`, ExpireDays: 1},
		{Query: "from:alice", Tags: []string{"from-alice"}},
	}}
	if err := saveConfig(env, next); err != nil {
		t.Fatal(err)
	}
	srv.Append(t, "INBOX", mail("v@x", "Your verification code"), daysAgo(env, 0), imap.FlagSeen)
	srv.Append(t, "INBOX", mail("n@x", "normal"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})

	if got := search(t, env, "tag:from-alice"); len(got) != 2 {
		t.Errorf("tag rule: %v", got)
	}
	if files(t, env.Paths.TransientArchive("personal")) != 1 {
		t.Error("expiring mail not kept out of the archive")
	}
	tomorrow := env.Now().AddDate(0, 0, 1)
	env.Now = func() time.Time { return tomorrow }
	runSyncOK(t, env, SyncIn{})
	if got := search(t, env, ""); !slices.Equal(got, []string{"normal"}) {
		t.Errorf("after expiry: %v", got)
	}
	if got := srv.UIDs(t, "INBOX"); len(got) != 1 {
		t.Errorf("expired message still on the server: %v", got)
	}
}

func TestPathsNeverBackUpTransient(t *testing.T) {
	env := newTestEnv(t)
	out, err := Call(t.Context(), env, Read, PathsShow, struct{}{})
	if err != nil || len(out.Backup.Never) != 1 || out.Backup.Never[0] != env.Paths.TransientDir {
		t.Errorf("paths = %+v, %v", out.Backup, err)
	}
}

func TestDeletionTagsNeedDeletePermission(t *testing.T) {
	env := newTestEnv(t)
	for _, add := range [][]string{{TagDeleted}, {"delete-after:2000-01-01"}} {
		_, err := Call(t.Context(), env, Tag, MsgTag, TagIn{Query: "", Add: add})
		if !errors.Is(err, ErrPermission) {
			t.Errorf("tag-level +%v: err = %v, want permission denied", add, err)
		}
	}
	if _, err := Call(t.Context(), env, Tag, MsgTag, TagIn{Query: "", Remove: []string{TagDeleted}, DryRun: true}); errors.Is(err, ErrPermission) {
		t.Errorf("removing the trash tag must stay at tag level: %v", err)
	}
}

func TestSearchHidesTrashUnlessAsked(t *testing.T) {
	for s, hidden := range map[string]bool{
		"":                         true,
		"from:deleted@x.org":       true,
		`"deleted items"`:          true,
		"tag:deleted":              false,
		"-tag:deleted":             false,
		"tag:inbox OR tag:deleted": false,
	} {
		q, err := parseWithoutTrash(s)
		if err != nil {
			t.Fatal(err)
		}
		plain, _ := query.Parse(s)
		if got := len(q.Groups) > len(plain.Groups); got != hidden {
			t.Errorf("%q: trash filter added = %v, want %v", s, got, hidden)
		}
	}
	if _, err := parseWithoutTrash("x OR"); err == nil {
		t.Error("a dangling OR must stay an error")
	}
}

// TestReindexPutsFilesBack: a move interrupted after the tags were saved
// leaves a trashed message in the backed-up archive; reindex fixes it.
func TestReindexPutsFilesBack(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	if _, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: idOf(t, env, "subject:\"read one\"")}); err != nil {
		t.Fatal(err)
	}
	transient, err := env.Transient("personal")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := env.Store("personal")
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	for e := range transient.All() {
		hash = e.Hash
	}
	if err := transient.MoveTo(archive, hash); err != nil { // the "crash"
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Delete, IndexRebuild, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if files(t, env.Paths.TransientArchive("personal")) != 1 {
		t.Error("reindex left trashed mail in the backed-up archive")
	}
	if got := search(t, env, "tag:deleted"); len(got) != 1 {
		t.Errorf("trash after reindex = %v", got)
	}
}

func TestDeleteNowCountsOnlyItsOwnMessages(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	// Another message is due too: it goes, but is not counted.
	if _, err := Call(t.Context(), env, Delete, MsgTag, TagIn{Query: idOf(t, env, "subject:\"read one\""),
		Add: []string{"delete-after:2000-01-01"}}); err != nil {
		t.Fatal(err)
	}
	q := idOf(t, env, "subject:\"sent three\"")

	release, err := acquireLock(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: q, Now: true}); !errors.Is(err, ErrBusy) {
		t.Errorf("delete now during a sync = %v, want busy", err)
	}
	release()

	out, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: q, Now: true})
	if err != nil || out.Matched != 1 || out.Purged != 1 || out.Pending != 0 {
		t.Fatalf("delete now = %+v, %v", out, err)
	}
	if got := search(t, env, `subject:"read one" OR subject:"sent three"`); len(got) != 0 {
		t.Errorf("left: %v", got)
	}
}

// Review: a dry run changes nothing on disk either. A dry-run delete moved
// the files into the transient store (never backed up) while the index
// still showed them as inbox mail.
func TestDryRunDeleteMovesNothing(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	files := func() []string {
		var out []string
		for _, dir := range []string{env.Paths.ArchiveDir, env.Paths.TransientDir} {
			_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					out = append(out, strings.TrimPrefix(p, env.Paths.DataDir))
				}
				return nil
			})
		}
		slices.Sort(out)
		return out
	}
	before, dump := files(), readFile(t, env.Paths.TagsDump)
	out, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: "subject:read", DryRun: true})
	if err != nil || out.Matched == 0 {
		t.Fatalf("dry run = %+v, %v", out, err)
	}
	if after := files(); !slices.Equal(after, before) {
		t.Errorf("files moved by a dry run:\n%v\nwant:\n%v", after, before)
	}
	if got := readFile(t, env.Paths.TagsDump); got != dump {
		t.Error("the tag dump changed in a dry run")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Review: tag, archive and delete see what search shows: trashed mail only
// when the query asks for it (tag:deleted) or names the message by id.
// Ids are accepted in upper case too.
func TestActionsMatchWhatSearchShows(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	id := idOf(t, env, `subject:"read one"`)
	if _, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: id}); err != nil {
		t.Fatal(err)
	}
	res, err := Call(t.Context(), env, Read, MsgSearch, SearchIn{Query: "from:alice"})
	if err != nil {
		t.Fatal(err)
	}
	shown := len(res.Messages)
	out, err := Call(t.Context(), env, Tag, MsgTag, TagIn{Query: "from:alice", Add: []string{"x"}, DryRun: true})
	if err != nil || out.Matched != shown {
		t.Errorf("msg tag matched %d, search shows %d (%v)", out.Matched, shown, err)
	}
	if out, err := Call(t.Context(), env, Tag, MsgTag, TagIn{Query: "from:alice tag:deleted", Add: []string{"x"}, DryRun: true}); err != nil || out.Matched != 1 {
		t.Errorf("asking for the trash: matched %d, %v", out.Matched, err)
	}
	if out, err := Call(t.Context(), env, Tag, MsgTag, TagIn{Query: strings.ToUpper(id), Add: []string{"x"}, DryRun: true}); err != nil || out.Matched != 1 {
		t.Errorf("upper-case id of a trashed message: matched %d, %v", out.Matched, err)
	}
}

// Review: the sidebar counted a tag's mail in the trash too, so a tag
// showed 1 next to a view of it that showed none. Tag counts leave out the
// trash, as searches do; the trash itself still counts its own.
func TestTagCountsMatchWhatSearchShows(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	if _, err := Call(t.Context(), env, Tag, MsgTag, TagIn{Query: "tag:inbox", Add: []string{"classify"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: `subject:"read one"`}); err != nil {
		t.Fatal(err)
	}
	counts, err := Call(t.Context(), env, Read, TagsList, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range counts {
		q := "tag:" + tc.Tag
		shown, err := Call(t.Context(), env, Read, MsgCount, CountIn{Query: q})
		if err != nil {
			t.Fatal(err)
		}
		if tc.Count != shown.Count {
			t.Errorf("tag %s counts %d, its search shows %d", tc.Tag, tc.Count, shown.Count)
		}
	}
}

// Review: purge worked from the list it took at the start, so a message
// restored while it ran (restore takes no lock) was deleted anyway. Each
// one is checked again just before its file goes.
func TestPurgeSkipsWhatWasRestoredMeanwhile(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	if _, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: `subject:"read one"`}); err != nil {
		t.Fatal(err)
	}
	later := env.Now().AddDate(0, 0, 60)
	env.Now = func() time.Time { return later }
	ix, err := env.Index(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	due, err := ix.PurgeDue(t.Context(), later.UTC().Format(dateLayout))
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %d, %v", len(due), err)
	}
	if _, err := Call(t.Context(), env, Tag, MsgRestore, RestoreIn{Query: "tag:deleted"}); err != nil {
		t.Fatal(err)
	}
	purged, err := purgeMessages(t.Context(), env, ix, due)
	if err != nil || len(purged) != 0 {
		t.Errorf("purged %v, %v", purged, err)
	}
	if got := search(t, env, `subject:"read one"`); len(got) != 1 {
		t.Errorf("the restored message is gone: %v", got)
	}
}

// Review: a trashed message fetched again (the server reset the folder,
// so it is fetched from the start) got a second file in the archive,
// which backups take, and purge removed only one of the two: the other
// came back on reindex.
func TestTrashedMailFetchedAgainStaysInTheTrash(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	srv.Append(t, "INBOX", mail("gone@x", "gone"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	if _, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: "subject:gone"}); err != nil {
		t.Fatal(err)
	}
	srv.Recreate(t, "INBOX") // a new UIDVALIDITY: fetched again from the start
	srv.Append(t, "INBOX", mail("gone@x", "gone"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	if got := search(t, env, "subject:gone tag:deleted"); len(got) != 1 {
		t.Fatalf("no longer in the trash: %v", got)
	}
	hash := store.Hash([]byte(mail("gone@x", "gone")))
	archive, err := env.Store("personal")
	if err != nil {
		t.Fatal(err)
	}
	if archive.Has(hash) {
		t.Error("a trashed message has a copy in the archive again")
	}
}

// A purge removes a message from both stores, should it be in both.
func TestPurgeRemovesEveryCopy(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	raw := []byte(mail("1@x", "read one"))
	if _, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: `subject:"read one"`, Now: false}); err != nil {
		t.Fatal(err)
	}
	archive, err := env.Store("personal")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := archive.Put(raw, "S"); err != nil { // a duplicate left by the bug above
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Delete, TrashEmpty, TrashEmptyIn{}); err != nil {
		t.Fatal(err)
	}
	transient, err := env.Transient("personal")
	if err != nil {
		t.Fatal(err)
	}
	if h := store.Hash(raw); archive.Has(h) || transient.Has(h) {
		t.Errorf("left after the purge: archive %v, transient %v", archive.Has(h), transient.Has(h))
	}
}
