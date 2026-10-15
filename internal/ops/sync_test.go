// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"nought/internal/config"
	"nought/internal/imaptest"
	"nought/internal/journal"
	"nought/internal/lock"
)

// syncEnv returns an Env with one account pointing at a fresh test server.
func syncEnv(t *testing.T, opts imaptest.Options, deletion bool) (*Env, *imaptest.Server) {
	t.Helper()
	env := newTestEnv(t)
	srv := imaptest.Start(t, opts)
	env.TLS = srv.Client
	cfg := srv.Config()
	addAccount(t, env, AccountIn{Name: "personal", Email: imaptest.User, IMAPHost: cfg.Host, IMAPPort: cfg.Port,
		Password: imaptest.Password})
	zero := 0
	setFolderRules(t, env, append(DefaultFolderRules(), config.FolderRule{Match: "Sent", KeepDays: &zero}))
	if deletion {
		if _, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "personal", Deletion: "enabled"}); err != nil {
			t.Fatal(err)
		}
	}
	return env, srv
}

func setFolderRules(t *testing.T, env *Env, rules []config.FolderRule) {
	t.Helper()
	cfg, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	next := &config.Config{Accounts: slices.Clone(cfg.Accounts)}
	next.Accounts[0].Folders = rules
	if err := saveConfig(env, next); err != nil {
		t.Fatal(err)
	}
}

func daysAgo(env *Env, d int) time.Time { return env.Now().Add(-time.Duration(d) * 24 * time.Hour) }

func runSyncOK(t *testing.T, env *Env, in SyncIn) SyncOut {
	t.Helper()
	out, err := Call(t.Context(), env, Delete, SyncRun, in)
	if err != nil {
		t.Fatalf("sync: %v\n%+v", err, out)
	}
	return out
}

func folder(t *testing.T, out SyncOut, name string) FolderSyncOut {
	t.Helper()
	for _, f := range out.Accounts[0].Folders {
		if f.Folder == name {
			return f
		}
	}
	t.Fatalf("folder %s not in %+v", name, out)
	return FolderSyncOut{}
}

func readJournal(t *testing.T, env *Env) []journal.Entry {
	t.Helper()
	f, err := os.Open(env.Paths.Journal)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []journal.Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e journal.Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func TestSyncArchivesWithoutDeletingByDefault(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	srv.Append(t, "INBOX", mail("1@x", "old read"), daysAgo(env, 30), imap.FlagSeen)
	srv.Append(t, "INBOX", mail("2@x", "fresh"), daysAgo(env, 1))
	srv.Append(t, "Sent", mail("3@x", "my reply"), daysAgo(env, 2), imap.FlagSeen)
	srv.Append(t, "Trash", mail("4@x", "trashed"), daysAgo(env, 2))
	srv.CreateMailbox(t, "Drafts")
	srv.Append(t, "Drafts", mail("5@x", "draft in progress"), daysAgo(env, 30))

	out := runSyncOK(t, env, SyncIn{})
	inbox := folder(t, out, "INBOX")
	if inbox.Fetched != 2 || inbox.Expunged != 0 || inbox.Due != 1 || len(inbox.Notes) != 1 {
		t.Errorf("INBOX = %+v", inbox)
	}
	if f := folder(t, out, "Trash"); f.Action != "ignore" || f.Fetched != 0 {
		t.Errorf("Trash = %+v", f)
	}
	if f := folder(t, out, "Drafts"); f.Action != "ignore" {
		t.Errorf("Drafts = %+v", f)
	}
	if got := search(t, env, "tag:inbox"); !slices.Equal(got, []string{"fresh", "old read"}) {
		t.Errorf("inbox = %v", got)
	}
	if got := search(t, env, "is:unread"); !slices.Equal(got, []string{"fresh"}) {
		t.Errorf("unread = %v", got)
	}
	if got := search(t, env, "tag:folder:sent -tag:inbox"); !slices.Equal(got, []string{"my reply"}) {
		t.Errorf("sent = %v", got)
	}
	if got := search(t, env, "trashed OR draft"); len(got) != 0 {
		t.Errorf("ignored folders archived: %v", got)
	}
	for _, mbox := range []string{"INBOX", "Sent"} {
		if n := len(srv.UIDs(t, mbox)); n == 0 {
			t.Errorf("%s emptied with deletion disabled", mbox)
		}
	}
	if dump, err := os.ReadFile(env.Paths.TagsDump); err != nil || !strings.Contains(string(dump), "id:2@x") {
		t.Errorf("tag dump not written: %v", err)
	}

	again := runSyncOK(t, env, SyncIn{})
	if f := folder(t, again, "INBOX"); f.New != 0 || f.Fetched != 0 {
		t.Errorf("second sync INBOX = %+v", f)
	}
}

func TestSyncDeletesOnlyDueVerifiedMessages(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	old := srv.Append(t, "INBOX", mail("1@x", "old"), daysAgo(env, 10))
	fresh := srv.Append(t, "INBOX", mail("2@x", "fresh"), daysAgo(env, 1))
	sent := srv.Append(t, "Sent", mail("3@x", "sent"), daysAgo(env, 1))
	// Marked for deletion by another client: never ours to expunge.
	other := srv.Append(t, "INBOX", mail("4@x", "other client"), daysAgo(env, 1))
	srv.SetDeleted(t, "INBOX", other)

	out := runSyncOK(t, env, SyncIn{})
	if f := folder(t, out, "INBOX"); f.Fetched != 3 || f.Expunged != 1 {
		t.Errorf("INBOX = %+v", f)
	}
	if f := folder(t, out, "Sent"); f.Expunged != 1 {
		t.Errorf("Sent = %+v", f)
	}
	if got := srv.UIDs(t, "INBOX"); !slices.Equal(got, []uint32{fresh, other}) {
		t.Errorf("INBOX on server = %v, want %v (old %d deleted)", got, []uint32{fresh, other}, old)
	}
	if got := srv.UIDs(t, "Sent"); len(got) != 0 {
		t.Errorf("Sent on server = %v (uid %d should be gone)", got, sent)
	}
	// Archived messages stay searchable; deletion is from the server only.
	if got := search(t, env, "old OR sent"); len(got) != 2 {
		t.Errorf("archive lost mail: %v", got)
	}

	entries := readJournal(t, env)
	if len(entries) != 4 {
		t.Fatalf("journal = %+v", entries)
	}
	for _, e := range entries {
		if e.Account != "personal" || len(e.Hash) != 64 || (e.Phase != journal.Intent && e.Phase != journal.Done) {
			t.Errorf("journal entry %+v", e)
		}
	}

	// Nothing more is due; a later sync deletes nothing else.
	if f := folder(t, runSyncOK(t, env, SyncIn{}), "INBOX"); f.Expunged != 0 || f.Due != 0 {
		t.Errorf("second sync INBOX = %+v", f)
	}
}

func TestSyncNeverDeletesWithoutVerifiedCopy(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	uid := srv.Append(t, "INBOX", mail("1@x", "precious"), daysAgo(env, 30))
	runSyncOK(t, env, SyncIn{})

	// Damage the local copy, then enable deletion.
	st, _ := env.Store("personal")
	for e := range st.All() {
		path := filepath.Join(env.Paths.AccountArchive("personal"), "cur", e.Hash+":2,")
		if err := os.WriteFile(path, []byte("bit rot"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "personal", Deletion: "enabled"}); err != nil {
		t.Fatal(err)
	}
	out, err := Call(t.Context(), env, Delete, SyncRun, SyncIn{})
	if !errors.Is(err, ErrPartial) {
		t.Fatalf("sync with damaged copy = %v", err)
	}
	if f := folder(t, out, "INBOX"); f.Expunged != 0 || f.Error == "" || len(f.Notes) == 0 {
		t.Errorf("INBOX = %+v", f)
	}
	if got := srv.UIDs(t, "INBOX"); !slices.Equal(got, []uint32{uid}) {
		t.Fatalf("message deleted without a verified copy: server has %v", got)
	}
	if entries := readJournal(t, env); len(entries) != 0 {
		t.Errorf("journal = %+v", entries)
	}
}

func TestSyncWithoutUIDPlusNeverDeletes(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{NoUIDPlus: true}, true)
	srv.Append(t, "INBOX", mail("1@x", "old"), daysAgo(env, 30))
	out := runSyncOK(t, env, SyncIn{})
	if f := folder(t, out, "INBOX"); f.Fetched != 1 || f.Expunged != 0 || len(f.Notes) != 1 || !strings.Contains(f.Notes[0], "UIDPLUS") {
		t.Errorf("INBOX = %+v", f)
	}
	if len(srv.UIDs(t, "INBOX")) != 1 {
		t.Error("deleted without UIDPLUS")
	}
}

func TestSyncDryRunChangesNothing(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	srv.Append(t, "INBOX", mail("1@x", "old"), daysAgo(env, 30))
	out := runSyncOK(t, env, SyncIn{DryRun: true})
	if f := folder(t, out, "INBOX"); f.New != 1 || f.Fetched != 0 || f.Expunged != 0 {
		t.Errorf("dry run INBOX = %+v", f)
	}
	if got := search(t, env, ""); len(got) != 0 {
		t.Errorf("dry run archived %v", got)
	}
	if len(srv.UIDs(t, "INBOX")) != 1 {
		t.Error("dry run deleted mail")
	}
}

func TestSyncUIDValidityChangeDoesNotDuplicate(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	srv.Append(t, "INBOX", mail("1@x", "one"), daysAgo(env, 1))
	runSyncOK(t, env, SyncIn{})

	// The server rebuilds the folder: same message, new UIDVALIDITY and UID.
	srv.Recreate(t, "INBOX")
	srv.Append(t, "INBOX", mail("1@x", "one"), daysAgo(env, 1))
	out := runSyncOK(t, env, SyncIn{})
	f := folder(t, out, "INBOX")
	if f.Fetched != 1 || len(f.Notes) == 0 || !strings.Contains(f.Notes[0], "UIDVALIDITY") {
		t.Errorf("INBOX = %+v", f)
	}
	st, _ := env.Store("personal")
	if st.Len() != 1 {
		t.Errorf("archive has %d copies", st.Len())
	}
	if got := search(t, env, ""); len(got) != 1 {
		t.Errorf("index has %v", got)
	}
}

func TestSyncKeepsTriageOfKnownMessages(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	srv.Append(t, "INBOX", mail("1@x", "triaged"), daysAgo(env, 1))
	runSyncOK(t, env, SyncIn{})
	if _, err := Call(t.Context(), env, Delete, MsgArchive, ArchiveIn{Query: "tag:inbox"}); err != nil {
		t.Fatal(err)
	}
	// After a UIDVALIDITY change the same message is fetched again; it must
	// not come back to the inbox.
	srv.Recreate(t, "INBOX")
	srv.Append(t, "INBOX", mail("1@x", "triaged"), daysAgo(env, 1))
	runSyncOK(t, env, SyncIn{})
	if got := search(t, env, "tag:inbox"); len(got) != 0 {
		t.Errorf("re-fetch undid triage: %v", got)
	}
}

func TestSyncResumesAfterBrokenConnection(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	for i, subj := range []string{"a", "b", "c"} {
		srv.Append(t, "INBOX", mail(subj+"@x", strings.Repeat(subj, 2000)), daysAgo(env, i))
	}
	srv.BreakNextConnection(6000)
	out, err := Call(t.Context(), env, Delete, SyncRun, SyncIn{})
	if !errors.Is(err, ErrPartial) {
		t.Fatalf("broken sync = %v, %+v", err, out)
	}
	runSyncOK(t, env, SyncIn{})
	if got := search(t, env, ""); len(got) != 3 {
		t.Errorf("after resume: %v", got)
	}
	st, _ := env.Store("personal")
	if st.Len() != 3 {
		t.Errorf("archive has %d files", st.Len())
	}
}

func TestSyncJournalFailureDeletesNothing(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	srv.Append(t, "INBOX", mail("1@x", "old"), daysAgo(env, 30))
	if err := os.MkdirAll(filepath.Join(env.Paths.Journal, "blocked"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := Call(t.Context(), env, Delete, SyncRun, SyncIn{})
	if !errors.Is(err, ErrPartial) {
		t.Fatalf("sync = %v", err)
	}
	if f := folder(t, out, "INBOX"); f.Expunged != 0 || !strings.Contains(f.Error, "nothing was deleted") {
		t.Errorf("INBOX = %+v", f)
	}
	if len(srv.UIDs(t, "INBOX")) != 1 {
		t.Fatal("deleted without a journal entry")
	}
}

func TestSyncErrors(t *testing.T) {
	env, _ := syncEnv(t, imaptest.Options{}, false)
	if _, err := Call(t.Context(), env, Delete, SyncRun, SyncIn{Accounts: []string{"nope"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown account = %v", err)
	}
	if _, err := Call(t.Context(), env, Tag, SyncRun, SyncIn{}); !errors.Is(err, ErrPermission) {
		t.Errorf("tag-level caller = %v", err)
	}
	l, err := lock.Acquire(env.Paths.LockFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Delete, SyncRun, SyncIn{}); !errors.Is(err, ErrBusy) {
		t.Errorf("concurrent sync = %v", err)
	}
	if _, err := Call(t.Context(), env, Delete, IndexRebuild, struct{}{}); !errors.Is(err, ErrBusy) {
		t.Errorf("reindex during sync = %v", err)
	}
	_ = l.Release()

	if err := env.Secrets.Set("personal", "wrong"); err != nil {
		t.Fatal(err)
	}
	out, err := Call(t.Context(), env, Delete, SyncRun, SyncIn{})
	if !errors.Is(err, ErrPartial) || out.Accounts[0].Error == "" || strings.Contains(out.Accounts[0].Error, "wrong") {
		t.Errorf("bad password = %v, %+v", err, out)
	}

	empty := newTestEnv(t)
	if _, err := Call(t.Context(), empty, Delete, SyncRun, SyncIn{}); !errors.Is(err, ErrUsage) {
		t.Errorf("no accounts = %v", err)
	}
}

func TestAccountTest(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	srv.CreateMailbox(t, "Junk")
	out, err := Call(t.Context(), env, Read, AccountTest, AccountName{Name: "personal"})
	if err != nil || !out.UIDPlus || len(out.Capabilities) == 0 {
		t.Fatalf("test = %+v, %v", out, err)
	}
	policies := map[string]string{}
	for _, f := range out.Folders {
		policies[f.Folder] = f.Policy.Action
	}
	if policies["INBOX"] != "archive" || policies["Junk"] != "ignore" || policies["Trash"] != "ignore" {
		t.Errorf("policies = %v", policies)
	}
	if _, err := Call(t.Context(), env, Read, AccountTest, AccountName{Name: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown account = %v", err)
	}
}
