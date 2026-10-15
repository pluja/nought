// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"nought/internal/config"
	"nought/internal/imaptest"
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
	if n, _ := purge(t.Context(), env); n != 0 {
		t.Fatal("purged before trash_days")
	}
	later := env.Now().AddDate(0, 0, config.DefaultTrashDays)
	env.Now = func() time.Time { return later }
	if n, err := purge(t.Context(), env); err != nil || n != 1 {
		t.Errorf("purge after trash_days = %d, %v", n, err)
	}

	empty, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: idOf(t, env, "subject:\"unread two\"")})
	if err != nil || empty.Matched != 1 {
		t.Fatal(err)
	}
	te, err := Call(t.Context(), env, Delete, TrashEmpty, struct{}{})
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
		if e.MessageID != "" {
			t.Errorf("journal keeps the Message-ID of deleted mail: %+v", e)
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
