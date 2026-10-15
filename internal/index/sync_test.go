// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
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
