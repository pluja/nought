// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pluja/nought/internal/imaptest"
)

// snapshot is everything a dry run must leave as it was: the archive files
// (names carry flags), the dumps, and the index's tags.
func snapshot(t *testing.T, env *Env) string {
	t.Helper()
	var b strings.Builder
	for _, dir := range []string{env.Paths.ArchiveDir, env.Paths.TransientDir} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				b.WriteString(path + "\n")
			}
			return err
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	for _, f := range []string{env.Paths.TagsDump, env.Paths.TransientDump} {
		data, _ := os.ReadFile(f)
		b.Write(data)
	}
	ix, err := env.Index(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var tags bytes.Buffer
	if err := ix.Dump(t.Context(), &tags, nil); err != nil {
		t.Fatal(err)
	}
	b.Write(tags.Bytes())
	return b.String()
}

// Each dry run reports what the real run then does, and changes nothing.
func TestDryRunsPredictAndChangeNothing(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	if _, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: "tag:inbox"}); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(t.TempDir(), "tags.dump")
	writeFile(t, dump, "+deleted -- id:3@x\n+inbox -- id:missing@x\n")
	before := snapshot(t, env)

	restore, err := Call(t.Context(), env, Tag, MsgRestore, RestoreIn{Query: "tag:deleted", DryRun: true})
	if err != nil || !restore.DryRun || restore.Changed != 3 {
		t.Errorf("restore dry run = %+v, %v", restore, err)
	}
	tags, err := Call(t.Context(), env, Delete, TagsRestore, TagsRestoreIn{Path: dump, DryRun: true})
	if err != nil || !tags.DryRun || tags.Applied != 1 || tags.Missing != 1 {
		t.Errorf("tags restore dry run = %+v, %v", tags, err)
	}
	empty, err := Call(t.Context(), env, Delete, TrashEmpty, TrashEmptyIn{DryRun: true})
	if err != nil || !empty.DryRun || empty.Purged != 3 || empty.Pending != 0 {
		t.Errorf("trash empty dry run = %+v, %v", empty, err)
	}
	if after := snapshot(t, env); after != before {
		t.Errorf("a dry run changed something:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	if got, err := Call(t.Context(), env, Delete, TrashEmpty, TrashEmptyIn{}); err != nil || got.Purged != 3 || got.DryRun {
		t.Errorf("trash empty = %+v, %v", got, err)
	}
}

// Mail still on a server waits for the next sync: the dry run says so.
func TestTrashEmptyDryRunCountsServerCopies(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	srv.Append(t, "INBOX", mail("code@x", "your code"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	if _, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: "subject:code"}); err != nil {
		t.Fatal(err)
	}
	got, err := Call(t.Context(), env, Delete, TrashEmpty, TrashEmptyIn{DryRun: true})
	if err != nil || got.Purged != 0 || got.Pending != 1 {
		t.Errorf("dry run = %+v, %v", got, err)
	}
}
