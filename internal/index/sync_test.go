// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"nought/internal/query"
)

func TestSyncState(t *testing.T) {
	ix, _ := openIndex(t)
	ctx := t.Context()
	if _, found, err := ix.SyncState(ctx, "a", "INBOX"); err != nil || found {
		t.Fatalf("new folder: found=%v err=%v", found, err)
	}
	if err := ix.SetSyncState(ctx, "a", "INBOX", SyncState{UIDValidity: 5, LastUID: 10}); err != nil {
		t.Fatal(err)
	}
	if err := ix.SetSyncState(ctx, "a", "INBOX", SyncState{UIDValidity: 5, LastUID: 12}); err != nil {
		t.Fatal(err)
	}
	st, found, err := ix.SyncState(ctx, "a", "INBOX")
	if err != nil || !found || st != (SyncState{UIDValidity: 5, LastUID: 12}) {
		t.Fatalf("state = %+v, %v, %v", st, found, err)
	}
}

func TestRecordCopyWithoutDateUsesFetchTime(t *testing.T) {
	ix, _ := openIndex(t)
	c := ServerCopy{Account: "a", Folder: "INBOX", UIDValidity: 1, UID: 1, Hash: hash(1), Size: 5}
	if err := ix.RecordCopy(t.Context(), c, now); err != nil {
		t.Fatal(err)
	}
	if due, _ := ix.DueCopies(t.Context(), "a", "INBOX", 1, now.Add(-time.Hour)); len(due) != 0 {
		t.Errorf("dateless message due before its grace period: %+v", due)
	}
	due, _ := ix.DueCopies(t.Context(), "a", "INBOX", 1, now)
	if len(due) != 1 || due[0].Size != 5 {
		t.Errorf("due = %+v", due)
	}
}

func TestMessageIDOf(t *testing.T) {
	ix, _ := openIndex(t)
	add(t, ix, Message{Account: "a", Hash: hash(1), MessageID: "m@x"}, "")
	if got := ix.MessageIDOf(t.Context(), "a", hash(1)); got != "m@x" {
		t.Errorf("MessageIDOf = %q", got)
	}
	if got := ix.MessageIDOf(t.Context(), "a", hash(2)); got != "" {
		t.Errorf("unknown = %q", got)
	}
}

func TestServerCopiesLifecycle(t *testing.T) {
	ix, _ := openIndex(t)
	ctx := t.Context()
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	copies := []ServerCopy{
		{Account: "a", Folder: "INBOX", UIDValidity: 5, UID: 1, Hash: hash(1), InternalDate: day(1)},
		{Account: "a", Folder: "INBOX", UIDValidity: 5, UID: 2, Hash: hash(2), InternalDate: day(10)},
		{Account: "a", Folder: "Sent", UIDValidity: 5, UID: 1, Hash: hash(3), InternalDate: day(1)},
	}
	for _, c := range copies {
		if err := ix.RecordCopy(ctx, c, now); err != nil {
			t.Fatal(err)
		}
		if err := ix.RecordCopy(ctx, c, now); err != nil { // idempotent
			t.Fatal(err)
		}
	}
	if err := ix.RecordCopy(ctx, ServerCopy{Hash: "bad"}, now); err == nil {
		t.Error("recorded a bad hash")
	}
	reused := copies[0]
	reused.Hash = hash(9)
	if err := ix.RecordCopy(ctx, reused, now); !errors.Is(err, ErrCopyConflict) {
		t.Errorf("reused uid = %v, want ErrCopyConflict", err)
	}

	due, err := ix.DueCopies(ctx, "a", "INBOX", 5, day(5))
	if err != nil || len(due) != 1 || due[0].UID != 1 || due[0].Hash != hash(1) || !due[0].InternalDate.Equal(day(1)) {
		t.Fatalf("due = %+v, %v", due, err)
	}
	if err := ix.MarkExpunged(ctx, due, now); err != nil {
		t.Fatal(err)
	}
	if due, _ := ix.DueCopies(ctx, "a", "INBOX", 5, day(30)); len(due) != 1 || due[0].UID != 2 {
		t.Errorf("after expunge, due = %+v", due)
	}
	if due, _ := ix.DueCopies(ctx, "a", "INBOX", 6, day(30)); len(due) != 0 {
		t.Errorf("other uidvalidity due = %+v", due)
	}

	// A UIDVALIDITY change forgets pending copies (they can never be
	// deleted by UID) but keeps the record of what was expunged.
	if err := ix.ResetFolder(ctx, "a", "INBOX", 6); err != nil {
		t.Fatal(err)
	}
	if due, _ := ix.DueCopies(ctx, "a", "INBOX", 5, day(30)); len(due) != 0 {
		t.Errorf("stale copies still due: %+v", due)
	}
	st, _, _ := ix.SyncState(ctx, "a", "INBOX")
	if st != (SyncState{UIDValidity: 6}) {
		t.Errorf("state after reset = %+v", st)
	}
	var expunged int
	if err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM server_copies WHERE expunged_at IS NOT NULL`).Scan(&expunged); err != nil || expunged != 1 {
		t.Errorf("expunged history = %d, %v", expunged, err)
	}
	if due, _ := ix.DueCopies(ctx, "a", "Sent", 5, day(30)); len(due) != 1 {
		t.Errorf("other folder affected: %+v", due)
	}
}

func TestOnServerAndStatus(t *testing.T) {
	ix, _ := openIndex(t)
	ctx := t.Context()
	st, err := ix.ServerStatus(ctx, "a")
	if err != nil || st.OnServer != 0 || !st.LastSynced.IsZero() {
		t.Fatalf("empty status = %+v, %v", st, err)
	}
	for _, folder := range []string{"INBOX", "Archive.2024"} {
		if err := ix.SetSyncState(ctx, "a", folder, SyncState{UIDValidity: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for i, folder := range []string{"INBOX", "Archive.2024"} {
		c := ServerCopy{Account: "a", Folder: folder, FolderName: strings.ReplaceAll(folder, ".", "/"), UIDValidity: 1,
			UID: uint32(i + 1), Hash: hash(1), Size: 5, InternalDate: now}
		if err := ix.RecordCopy(ctx, c, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := ix.RecordCopy(ctx, ServerCopy{Account: "a", Folder: "INBOX", UIDValidity: 1, UID: 9, Hash: hash(2), Size: 5, InternalDate: now}, now); err != nil {
		t.Fatal(err)
	}
	gone, _ := ix.DueCopies(ctx, "a", "INBOX", 1, now)
	if err := ix.MarkExpunged(ctx, gone[1:], now); err != nil { // hash(2) left the server
		t.Fatal(err)
	}
	on, err := ix.OnServer(ctx, "a", []string{hash(1), hash(2), hash(3)})
	if err != nil || len(on[hash(1)]) != 2 || on[hash(1)][0].FolderName != "Archive/2024" || len(on[hash(2)]) != 0 {
		t.Fatalf("OnServer = %+v, %v", on, err)
	}
	if err := ix.SetSyncState(ctx, "a", "INBOX", SyncState{UIDValidity: 1, LastUID: 9}); err != nil {
		t.Fatal(err)
	}
	if err := ix.MarkSynced(ctx, "a", "INBOX", now); err != nil {
		t.Fatal(err)
	}
	if err := ix.MarkSynced(ctx, "a", "Archive.2024", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	st, err = ix.ServerStatus(ctx, "a")
	if err != nil || st.OnServer != 1 || !st.LastSynced.Equal(now.Add(-time.Hour)) { // the oldest folder
		t.Errorf("status = %+v, %v", st, err)
	}
}

func TestFolderNameBackfill(t *testing.T) {
	ix, _ := openIndex(t)
	ctx := t.Context()
	if err := ix.SetSyncState(ctx, "a", "Archive.2024", SyncState{UIDValidity: 1}); err != nil {
		t.Fatal(err)
	}
	// A row as written before migration 0003.
	if _, err := ix.db.ExecContext(ctx, `INSERT INTO server_copies (account, folder, folder_name, uidvalidity, uid, hash, size, internal_date, fetched_at)
		VALUES ('a', 'Archive.2024', '', 1, 1, ?, 5, 0, 0)`, hash(1)); err != nil {
		t.Fatal(err)
	}
	on, err := ix.OnServer(ctx, "a", []string{hash(1)})
	if err != nil || len(on[hash(1)]) != 1 || on[hash(1)][0].FolderName != "Archive.2024" {
		t.Fatalf("fallback = %+v, %v", on, err)
	}
	// Migration 0004 fills the column itself.
	script, err := migrations.ReadFile("migrations/0004_folder_name_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.db.ExecContext(ctx, string(script)); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := ix.db.QueryRowContext(ctx, "SELECT folder_name FROM server_copies").Scan(&name); err != nil || name != "Archive.2024" {
		t.Errorf("backfilled name = %q, %v", name, err)
	}
}

func TestDeletionBookkeeping(t *testing.T) {
	ix, _ := openIndex(t)
	ctx := t.Context()
	add(t, ix, Message{Account: "a", Hash: hash(1), MessageID: "keep@x", Tags: []string{"inbox"}}, "")
	add(t, ix, Message{Account: "a", Hash: hash(2), MessageID: "trash@x", Tags: []string{"deleted", "delete-after:2026-11-14"}}, "")
	add(t, ix, Message{Account: "a", Hash: hash(3), MessageID: "code@x", Tags: []string{"delete-after:2026-10-15"}}, "")
	if err := ix.SetSyncState(ctx, "a", "INBOX", SyncState{UIDValidity: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if err := ix.RecordCopy(ctx, ServerCopy{Account: "a", Folder: "INBOX", UIDValidity: 1, UID: uint32(i), Hash: hash(i), Size: 1, InternalDate: now}, now); err != nil {
			t.Fatal(err)
		}
	}
	req, err := ix.DeletionRequested(ctx, "a", "INBOX", 1, "2026-10-15")
	if err != nil || len(req) != 2 || req[0].UID != 2 || req[1].UID != 3 {
		t.Fatalf("deletion requests = %+v, %v", req, err)
	}
	if req, _ := ix.DeletionRequested(ctx, "a", "INBOX", 1, "2026-10-14"); len(req) != 1 {
		t.Errorf("before the date, only the trashed one: %+v", req)
	}
	due, err := ix.PurgeDue(ctx, "2026-10-15")
	if err != nil || len(due) != 1 || due[0].MessageID != "code@x" {
		t.Fatalf("purge due = %+v, %v", due, err)
	}
	if err := ix.DeleteMessage(ctx, due[0]); err != nil {
		t.Fatal(err)
	}
	if ok, _ := ix.HasHash(ctx, "a", hash(3)); ok {
		t.Error("message still indexed")
	}
	if on, _ := ix.OnServer(ctx, "a", []string{hash(3)}); len(on[hash(3)]) != 0 {
		t.Error("server copy record kept")
	}
	parsed, _ := query.Parse("code")
	if n, _ := ix.Count(ctx, parsed); n != 0 {
		t.Error("full-text entry kept")
	}

	var all, transient strings.Builder
	if err := ix.Dump(ctx, &all, nil); err != nil {
		t.Fatal(err)
	}
	if err := ix.Dump(ctx, &transient, func(tags []string) bool { return slices.Contains(tags, "deleted") }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(all.String(), "keep@x") || strings.Contains(transient.String(), "keep@x") || !strings.Contains(transient.String(), "trash@x") {
		t.Errorf("dumps:\n%s\n---\n%s", all.String(), transient.String())
	}
}

func TestExpiryTagsMustBeDates(t *testing.T) {
	for _, tag := range []string{"delete-after:", "delete-after:1d", "delete-after:2026-1-5", "delete-after:0",
		"delete-after:2026-02-30", "delete-after:2026-10-15x"} {
		if ValidateTag(tag) == nil {
			t.Errorf("ValidateTag(%q) accepted a malformed date", tag)
		}
	}
	if err := ValidateTag("delete-after:2026-10-15"); err != nil {
		t.Error(err)
	}
}

func TestVanishedAndHandledCopies(t *testing.T) {
	ix, _ := openIndex(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	add := func(hash string, tags []string, uid uint32) {
		t.Helper()
		if _, _, err := ix.AddMessage(ctx, Message{Account: "a", Hash: hash, Tags: tags}, "", now); err != nil {
			t.Fatal(err)
		}
		if err := ix.SetSyncState(ctx, "a", "INBOX", SyncState{UIDValidity: 1, LastUID: uid}); err != nil {
			t.Fatal(err)
		}
		if err := ix.RecordCopy(ctx, ServerCopy{Account: "a", Folder: "INBOX", UIDValidity: 1, UID: uid, Hash: hash,
			Size: 10, InternalDate: now.Add(-48 * time.Hour)}, now); err != nil {
			t.Fatal(err)
		}
	}
	inbox, archived := strings.Repeat("a", 64), strings.Repeat("b", 64)
	add(inbox, []string{"inbox"}, 1)
	add(archived, nil, 2)

	handled, err := ix.HandledCopies(ctx, "a", "INBOX", 1, now)
	if err != nil || len(handled) != 1 || handled[0].Hash != archived {
		t.Fatalf("handled = %+v, %v", handled, err)
	}
	if h, _ := ix.HandledCopies(ctx, "a", "INBOX", 1, now.Add(-72*time.Hour)); len(h) != 0 {
		t.Errorf("keep_days not respected: %+v", h)
	}
	live, _ := ix.LiveCopies(ctx, "a", "INBOX", 1)
	if len(live) != 2 {
		t.Fatalf("live = %+v", live)
	}
	if f, _ := ix.TrackedFolders(ctx, "a"); !maps.Equal(f, map[string]uint32{"INBOX": 1}) {
		t.Errorf("tracked = %v", f)
	}
	if err := ix.MarkVanished(ctx, live, now); err != nil {
		t.Fatal(err)
	}
	live, _ = ix.LiveCopies(ctx, "a", "INBOX", 1)
	handled, _ = ix.HandledCopies(ctx, "a", "INBOX", 1, now)
	due, _ := ix.DueCopies(ctx, "a", "INBOX", 1, now)
	on, _ := ix.OnServer(ctx, "a", []string{inbox, archived})
	st, _ := ix.ServerStatus(ctx, "a")
	tracked, _ := ix.TrackedFolders(ctx, "a")
	if len(live)+len(handled)+len(due)+len(on)+st.OnServer+len(tracked) != 0 {
		t.Errorf("vanished copies still counted: live %d handled %d due %d on %v status %d tracked %v",
			len(live), len(handled), len(due), on, st.OnServer, tracked)
	}
}

func TestSyncedUntilTheFolderRestarts(t *testing.T) {
	ix, _ := openIndex(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	synced := func() bool {
		t.Helper()
		st, _, err := ix.SyncState(ctx, "a", "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		return st.Synced
	}
	if err := ix.ResetFolder(ctx, "a", "INBOX", 1); err != nil {
		t.Fatal(err)
	}
	for _, restart := range []func() error{
		func() error { return ix.ResetFolder(ctx, "a", "INBOX", 2) },
		func() error { return ix.ForgetFolder(ctx, "a", "INBOX", 2) },
	} {
		if err := ix.MarkSynced(ctx, "a", "INBOX", now); err != nil || !synced() {
			t.Fatalf("not synced after MarkSynced: %v", err)
		}
		if err := restart(); err != nil || synced() {
			t.Errorf("still synced after a restart: %v", err)
		}
	}
}
