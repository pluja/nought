// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"errors"
	"strings"
	"testing"
	"time"
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
	if err != nil || !found || st != (SyncState{5, 12}) {
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
	ix, path := openIndex(t)
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
	// Re-running migration 0004 fills the column itself.
	if _, err := ix.db.ExecContext(ctx, "PRAGMA user_version = 3"); err != nil {
		t.Fatal(err)
	}
	_ = ix.Close()
	again, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	var name string
	if err := again.db.QueryRowContext(ctx, "SELECT folder_name FROM server_copies").Scan(&name); err != nil || name != "Archive.2024" {
		t.Errorf("backfilled name = %q, %v", name, err)
	}
}
