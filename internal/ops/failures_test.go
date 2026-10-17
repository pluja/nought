// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
