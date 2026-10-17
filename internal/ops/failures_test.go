// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pluja/nought/internal/index"
	"github.com/pluja/nought/internal/smtptest"
)

func TestDescribeOtherDevices(t *testing.T) {
	for _, c := range []struct {
		archived, deleted, back int
		want                    string
	}{
		{0, 0, 0, ""},
		{2, 0, 0, "2 archived"},
		{0, 1, 0, "1 deleted"},
		{2, 1, 3, "2 archived, 1 deleted, 3 moved back to the inbox"},
	} {
		if got := DescribeOtherDevices(c.archived, c.deleted, c.back); got != c.want {
			t.Errorf("DescribeOtherDevices(%d, %d, %d) = %q, want %q", c.archived, c.deleted, c.back, got, c.want)
		}
	}
}

// archivedFile is the path of a message's file in the archive.
func archivedFile(t *testing.T, env *Env, id string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(env.Paths.AccountArchive("personal"), "*", id+"*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("archived file of %s: %v, %v", id, files, err)
	}
	return files[0]
}

// A message whose file went missing says so (and how to repair it), and
// does not stop tagging it.
func TestMissingFileIsReported(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	id := idOf(t, env, `subject:"read one"`)
	if err := os.Remove(archivedFile(t, env, id)); err != nil {
		t.Fatal(err)
	}
	_, err := Call(t.Context(), env, Read, MsgShow, ShowIn{ID: id})
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "nou reindex") {
		t.Errorf("show = %v", err)
	}
	if _, err := Call(t.Context(), env, Tag, MsgTag, TagIn{Query: id, Add: []string{TagFlagged}}); err != nil {
		t.Errorf("tag = %v", err)
	}
}

// Files move to the transient store only once the dump records the tag
// that moves them: if the dump cannot be written, they stay.
func TestFilesStayWhenTheDumpCannotBeWritten(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	id := idOf(t, env, `subject:"read one"`)
	file := archivedFile(t, env, id)
	if err := errors.Join(os.Remove(env.Paths.TagsDump), os.MkdirAll(filepath.Join(env.Paths.TagsDump, "block"), 0o700)); err != nil {
		t.Fatal(err)
	}
	_, err := Call(t.Context(), env, Delete, MsgTag, TagIn{Query: id, Add: []string{TagDeleted}})
	if !errors.Is(err, ErrPartial) {
		t.Fatalf("tag with an unwritable dump = %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("the file moved before the dump recorded why: %v", err)
	}
}

// A password stored for an account that could not be saved is removed
// again: no credential is left for an account that does not exist.
func TestFailedAccountAddLeavesNoPassword(t *testing.T) {
	env := newTestEnv(t)
	addAccount(t, env, AccountIn{Name: "first", Email: "a@example.org", IMAPHost: "imap.example.org", IMAPPort: 993, Password: "x"})
	if err := errors.Join(os.Remove(env.Paths.ConfigFile), os.MkdirAll(filepath.Join(env.Paths.ConfigFile, "block"), 0o700)); err != nil {
		t.Fatal(err)
	}
	_, err := Call(t.Context(), env, Delete, AccountAdd, AccountIn{Name: "second", Email: "b@example.org",
		IMAPHost: "imap.example.org", IMAPPort: 993, Password: "secret"})
	if err == nil {
		t.Fatal("added an account without saving the config")
	}
	if pw, err := env.Secrets.Get("second"); err == nil {
		t.Errorf("password %q left for an unsaved account", pw)
	}
}

// When the index cannot be opened, every op that needs it fails, rather
// than report nothing found, and none touches the tag dump. (Sending
// does not need it: queued mail waits in the outbox.)
func TestUnusableIndexFails(t *testing.T) {
	env, _ := sendEnv(t, smtptest.Options{})
	id := idOf(t, env, `subject:"read one"`)
	dump, err := os.ReadFile(env.Paths.TagsDump)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "tags.dump")
	writeFile(t, backup, string(dump))
	if err := errors.Join(env.closeIndex(), removeDB(env.Paths.IndexFile), os.MkdirAll(env.Paths.IndexFile, 0o700)); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	out := filepath.Join(t.TempDir(), "out")
	calls := map[string]func() error{
		"search":  func() error { _, err := Call(ctx, env, Read, MsgSearch, SearchIn{Query: "tag:inbox"}); return err },
		"count":   func() error { _, err := Call(ctx, env, Read, MsgCount, CountIn{Query: "tag:inbox"}); return err },
		"show":    func() error { _, err := Call(ctx, env, Read, MsgShow, ShowIn{ID: id}); return err },
		"save":    func() error { _, err := Call(ctx, env, Read, MsgSave, SaveIn{ID: id, Dir: out}); return err },
		"tag":     func() error { _, err := Call(ctx, env, Tag, MsgTag, TagIn{Query: id, Add: []string{"x"}}); return err },
		"archive": func() error { _, err := Call(ctx, env, Tag, MsgArchive, ArchiveIn{Query: id}); return err },
		"delete":  func() error { _, err := Call(ctx, env, Delete, MsgDelete, DeleteIn{Query: id}); return err },
		"restore": func() error { _, err := Call(ctx, env, Delete, MsgRestore, RestoreIn{Query: id}); return err },
		"empty":   func() error { _, err := Call(ctx, env, Delete, TrashEmpty, TrashEmptyIn{}); return err },
		"tags":    func() error { _, err := Call(ctx, env, Read, TagsList, struct{}{}); return err },
		"dump":    func() error { _, err := Call(ctx, env, Read, TagsDump, TagsDumpIn{Path: out}); return err },
		"undump":  func() error { _, err := Call(ctx, env, Delete, TagsRestore, TagsRestoreIn{Path: backup}); return err },
		"import": func() error {
			_, err := Call(ctx, env, Delete, ImportRun, ImportIn{Path: thunderbirdExport(t), Account: "personal"})
			return err
		},
		"reply":   func() error { _, err := Call(ctx, env, Draft, DraftReply, DraftReplyIn{ID: id}); return err },
		"forward": func() error { _, err := Call(ctx, env, Draft, DraftForward, DraftForwardIn{ID: id}); return err },
		"suggest": func() error { _, err := Call(ctx, env, Read, AddressSuggest, SuggestIn{Prefix: "a"}); return err },
		"status":  func() error { _, err := Call(ctx, env, Read, AccountStatus, struct{}{}); return err },
		"reindex": func() error { _, err := Call(ctx, env, Delete, IndexRebuild, struct{}{}); return err },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s succeeded without an index", name)
		}
	}
	if got, err := os.ReadFile(env.Paths.TagsDump); err != nil || string(got) != string(dump) {
		t.Errorf("the tag dump changed: %v", err)
	}
}

func TestLeavesServerAt(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		tags   []string
		want   time.Time
		leaves bool
	}{
		{nil, time.Time{}, false},
		{[]string{"inbox"}, time.Time{}, false},
		{[]string{TagDeleted}, now, true},
		{[]string{index.DeleteAfterPrefix + "2026-10-20"}, time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), true},
		{[]string{index.DeleteAfterPrefix + "2026-01-01"}, now, true}, // overdue: at the next sync
	} {
		got, leaves := leavesServerAt(c.tags, now)
		if leaves != c.leaves || !got.Equal(c.want) {
			t.Errorf("leavesServerAt(%v) = %v, %v; want %v, %v", c.tags, got, leaves, c.want, c.leaves)
		}
	}
}

// Mail imported as deleted goes to the transient store, which backups skip,
// like mail deleted in nought.
func TestImportAsDeletedIsTransient(t *testing.T) {
	env := newTestEnv(t)
	out, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: thunderbirdExport(t), Account: "personal", Tags: []string{TagDeleted}})
	if err != nil || out.Imported != 3 {
		t.Fatalf("import = %+v, %v", out, err)
	}
	for dir, want := range map[string]int{env.Paths.AccountArchive("personal"): 0, env.Paths.TransientArchive("personal"): 3} {
		got, err := filepath.Glob(filepath.Join(dir, "cur", "*"))
		if err != nil || len(got) != want {
			t.Errorf("%s has %d files, want %d (%v)", dir, len(got), want, err)
		}
	}
}
