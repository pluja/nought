// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pluja/nought/internal/query"
)

// A file that is not an index is refused and left as it was: it may be
// the user's, and the index can always be rebuilt elsewhere.
func TestOpenRefusesADamagedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	garbage := bytes.Repeat([]byte("not a database\n"), 512)
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if ix, err := Open(t.Context(), path); err == nil {
		_ = ix.Close()
		t.Fatal("opened a file that is not a database")
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, garbage) {
		t.Errorf("the damaged file changed: %d bytes, %v", len(got), err)
	}
}

// Every method fails on a closed index, rather than reporting success (or
// nothing found, which callers would act on).
func TestClosedIndexFails(t *testing.T) {
	ix, _ := openIndex(t)
	sample(t, ix)
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	c := ServerCopy{Account: "personal", Folder: "INBOX", UIDValidity: 1, UID: 1, Hash: hash(1), Size: 1, InternalDate: now}
	calls := map[string]func() error{
		"AddMessage": func() error {
			_, _, err := ix.AddMessage(ctx, Message{Account: "personal", Hash: hash(9)}, "", now)
			return err
		},
		"Candidates":    func() error { _, err := ix.Candidates(ctx, "personal", hash(1), "one@x"); return err },
		"Resolve":       func() error { _, err := ix.Resolve(ctx, "personal", "1111"); return err },
		"Get":           func() error { _, err := ix.Get(ctx, 1); return err },
		"DeleteMessage": func() error { return ix.DeleteMessage(ctx, Message{ID: 1, Account: "personal", Hash: hash(1)}) },
		"PurgeDue":      func() error { _, err := ix.PurgeDue(ctx, "2026-10-15"); return err },
		"SyncState":     func() error { _, _, err := ix.SyncState(ctx, "personal", "INBOX"); return err },
		"SetSyncState":  func() error { return ix.SetSyncState(ctx, "personal", "INBOX", SyncState{UIDValidity: 1}) },
		"ResetFolder":   func() error { return ix.ResetFolder(ctx, "personal", "INBOX", 1) },
		"ForgetFolder":  func() error { return ix.ForgetFolder(ctx, "personal", "INBOX", 1) },
		"MarkSynced":    func() error { return ix.MarkSynced(ctx, "personal", "INBOX", now) },
		"Search":        func() error { _, err := ix.Search(ctx, query.Query{}, 10, 0); return err },
		"Each":          func() error { return ix.Each(ctx, query.Query{}, func([]Message) error { return nil }) },
		"Count":         func() error { _, err := ix.Count(ctx, query.Query{}); return err },
		"Addresses":     func() error { _, err := ix.Addresses(ctx, "personal", 10); return err },
		"ModifyTags":    func() error { return ix.ModifyTags(ctx, []int64{1}, []string{"x"}, []string{"inbox"}) },
		"TagCounts":     func() error { _, err := ix.TagCounts(ctx); return err },
		"Dump":          func() error { return ix.Dump(ctx, &bytes.Buffer{}, nil) },
		"Restore": func() error {
			_, err := ix.Restore(ctx, strings.NewReader("+inbox -- id:one@x\n"))
			return err
		},
		"RecordCopy":        func() error { return ix.RecordCopy(ctx, c, now) },
		"ExpiredCopies":     func() error { _, err := ix.ExpiredCopies(ctx, "personal", "INBOX", 1, now); return err },
		"LiveCopies":        func() error { _, err := ix.LiveCopies(ctx, "personal", "INBOX", 1); return err },
		"HandledCopies":     func() error { _, err := ix.HandledCopies(ctx, "personal", "INBOX", 1, now); return err },
		"MarkVanished":      func() error { return ix.MarkVanished(ctx, []ServerCopy{c}, now) },
		"TrackedFolders":    func() error { _, err := ix.TrackedFolders(ctx, "personal"); return err },
		"MarkExpunged":      func() error { return ix.MarkExpunged(ctx, []ServerCopy{c}, now) },
		"OnServer":          func() error { _, err := ix.OnServer(ctx, "personal", []string{hash(1)}); return err },
		"ServerStatus":      func() error { _, err := ix.ServerStatus(ctx, "personal"); return err },
		"DeletionRequested": func() error { _, err := ix.DeletionRequested(ctx, "personal", "INBOX", 1, "2026-10-15"); return err },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s succeeded on a closed index", name)
		}
	}
	if got := ix.MessageIDOf(ctx, "personal", hash(1)); got != "" {
		t.Errorf("MessageIDOf = %q on a closed index", got)
	}
}
