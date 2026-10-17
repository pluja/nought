// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package ops

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A rebuild that cannot read every message is not installed: the new index
// would lack those messages, and the next dump written from it would drop
// their tags, whose only lasting copy the dump is.
func TestReindexKeepsTheIndexWhenAMessageIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	env := newTestEnv(t)
	importSample(t, env)
	id := idOf(t, env, `subject:"read one"`)
	if _, err := Call(t.Context(), env, Tag, MsgTag, TagIn{Query: id, Add: []string{"work"}}); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(env.Paths.AccountArchive("personal"), "*", id+"*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("archived file of %s: %v, %v", id, files, err)
	}
	if err := os.Chmod(files[0], 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(files[0], 0o600) })

	out, err := Call(t.Context(), env, Delete, IndexRebuild, struct{}{})
	if !errors.Is(err, ErrPartial) || out.Failed != 1 || len(out.Failures) != 1 {
		t.Fatalf("reindex with an unreadable message = %+v, %v", out, err)
	}
	if _, err := os.Stat(env.Paths.IndexFile + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Error("the refused index was left behind")
	}
	if err := os.Chmod(files[0], 0o600); err != nil {
		t.Fatal(err)
	}
	// The old index is in place, and a later rebuild still has the tag.
	if got := search(t, env, "tag:work"); !slices.Equal(got, []string{"read one"}) {
		t.Errorf("tag:work after the refused rebuild = %v", got)
	}
	if _, err := Call(t.Context(), env, Tag, MsgTag, TagIn{Query: "tag:inbox", Add: []string{"seen-later"}}); err != nil {
		t.Fatal(err) // writes the dump again
	}
	if _, err := Call(t.Context(), env, Delete, IndexRebuild, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if got := search(t, env, "tag:work"); !slices.Equal(got, []string{"read one"}) {
		t.Errorf("tag:work after a full rebuild = %v", got)
	}
}

// The same holds for the rebuild of a missing index: nou refuses to start
// with an index that lacks a message, rather than drop its tags later.
func TestMissingIndexIsNotRebuiltWithoutAMessage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	env := newTestEnv(t)
	importSample(t, env)
	id := idOf(t, env, `subject:"read one"`)
	if _, err := Call(t.Context(), env, Tag, MsgTag, TagIn{Query: id, Add: []string{"work"}}); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(env.Paths.AccountArchive("personal"), "*", id+"*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("archived file of %s: %v, %v", id, files, err)
	}
	if err := errors.Join(env.closeIndex(), removeDB(env.Paths.IndexFile), os.Chmod(files[0], 0o000)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(files[0], 0o600) })

	if _, err := Call(t.Context(), env, Read, MsgSearch, SearchIn{Query: "tag:work"}); err == nil {
		t.Fatal("started with an index missing a message")
	}
	if err := os.Chmod(files[0], 0o600); err != nil {
		t.Fatal(err)
	}
	if got := search(t, env, "tag:work"); !slices.Equal(got, []string{"read one"}) {
		t.Errorf("tag:work once the file is readable = %v", got)
	}
}
