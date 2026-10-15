// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"nought/internal/query"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mail(id, subject string) string {
	return "From: alice@example.org\r\nTo: me@example.org\r\nMessage-ID: <" + id + ">\r\n" +
		"Date: Tue, 14 Oct 2026 09:30:00 +0000\r\nSubject: " + subject + "\r\n\r\nhello " + subject + "\r\n"
}

// thunderbirdExport builds a small Maildir export with a nested folder, a
// read message, and a Thunderbird-deleted message.
func thunderbirdExport(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "export")
	writeFile(t, filepath.Join(root, "Inbox", "cur", "1:2,S"), mail("1@x", "read one"))
	writeFile(t, filepath.Join(root, "Inbox", "cur", "2:2,"), mail("2@x", "unread two"))
	writeFile(t, filepath.Join(root, "Sent Items", "cur", "3:2,S"), mail("3@x", "sent three"))
	writeFile(t, filepath.Join(root, "Inbox", "cur", "4:2,"), "X-Mozilla-Status: 0008\r\n"+mail("4@x", "deleted"))
	return root
}

func search(t *testing.T, env *Env, q string) []string {
	t.Helper()
	ix, err := env.Index(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := query.Parse(q)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := ix.Search(t.Context(), parsed, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range msgs {
		out = append(out, m.Subject)
	}
	slices.Sort(out)
	return out
}

func TestImportMaildir(t *testing.T) {
	env := newTestEnv(t)
	root := thunderbirdExport(t)

	dry, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: root, Account: "personal", DryRun: true})
	if err != nil || dry.Imported != 3 || dry.Deleted != 1 || !dry.DryRun {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
	if got := search(t, env, ""); len(got) != 0 {
		t.Fatalf("dry run imported %v", got)
	}

	out, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: root, Account: "personal", Tags: []string{"imported"}})
	if err != nil || out.Imported != 3 || out.Deleted != 1 || out.Duplicates != 0 {
		t.Fatalf("import = %+v, %v", out, err)
	}
	checks := map[string][]string{
		"tag:imported":          {"read one", "sent three", "unread two"},
		"is:unread":             {"unread two"},
		"tag:folder:sent-items": {"sent three"},
		"tag:folder:inbox":      {"read one", "unread two"},
		"tag:inbox":             nil,
		"hello":                 {"read one", "sent three", "unread two"},
	}
	for q, want := range checks {
		if got := search(t, env, q); !slices.Equal(got, want) {
			t.Errorf("search %q = %v, want %v", q, got, want)
		}
	}
	dump, err := os.ReadFile(env.Paths.TagsDump)
	if err != nil || !strings.Contains(string(dump), "-- id:2@x") {
		t.Errorf("tags dump not written: %v\n%s", err, dump)
	}

	again, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: root, Account: "personal"})
	if err != nil || again.Imported != 0 || again.Duplicates != 3 {
		t.Errorf("re-import = %+v, %v", again, err)
	}
}

func TestImportMboxDedupesByMessageID(t *testing.T) {
	env := newTestEnv(t)
	root := thunderbirdExport(t)
	if _, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: root, Account: "personal"}); err != nil {
		t.Fatal(err)
	}
	// The same message with a different Thunderbird header is a duplicate by
	// Message-ID; a new one is imported, with --inbox.
	mbox := filepath.Join(t.TempDir(), "Archive")
	writeFile(t, mbox, "From - x\r\nX-Mozilla-Status: 0001\r\n"+mail("1@x", "read one")+"\r\nFrom - y\r\n"+mail("9@x", "new nine"))
	out, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: mbox, Account: "personal", Inbox: true})
	if err != nil || out.Imported != 1 || out.Duplicates != 1 {
		t.Fatalf("mbox import = %+v, %v", out, err)
	}
	// The duplicate keeps one copy but gains this import's tags.
	if got := search(t, env, "tag:inbox tag:folder:archive"); !slices.Equal(got, []string{"new nine", "read one"}) {
		t.Errorf("inbox = %v", got)
	}
}

func TestImportDuplicateMergesFolderTags(t *testing.T) {
	env := newTestEnv(t)
	root := filepath.Join(t.TempDir(), "export")
	writeFile(t, filepath.Join(root, "Inbox", "a.eml"), mail("1@x", "filed twice"))
	writeFile(t, filepath.Join(root, "Projects", "a.eml"), "X-Mozilla-Status: 0001\r\n"+mail("1@x", "filed twice"))
	// Same Message-ID, different sender: not a duplicate.
	writeFile(t, filepath.Join(root, "Other", "b.eml"), strings.Replace(mail("1@x", "forged id"), "alice@", "mallory@", 1))
	out, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: root, Account: "personal"})
	if err != nil || out.Imported != 2 || out.Duplicates != 1 {
		t.Fatalf("import = %+v, %v", out, err)
	}
	if got := search(t, env, "tag:folder:inbox tag:folder:projects"); !slices.Equal(got, []string{"filed twice"}) {
		t.Errorf("merged folder tags = %v", got)
	}
	if got := search(t, env, "subject:forged"); len(got) != 1 {
		t.Errorf("forged Message-ID hid a message: %v", got)
	}
}

func TestImportErrors(t *testing.T) {
	env := newTestEnv(t)
	tests := []struct {
		in   ImportIn
		want error
	}{
		{ImportIn{Account: "personal"}, ErrUsage},
		{ImportIn{Path: "/x"}, ErrUsage},
		{ImportIn{Path: t.TempDir(), Account: "../evil"}, ErrUsage},
		{ImportIn{Path: t.TempDir(), Account: "a", Tags: []string{"Bad Tag"}}, ErrUsage},
		{ImportIn{Path: filepath.Join(t.TempDir(), "missing"), Account: "a"}, ErrUsage},
		{ImportIn{Path: filepath.Join(t.TempDir(), "missing"), Account: "a", Format: "mbox"}, ErrNotFound},
		{ImportIn{Path: t.TempDir(), Account: "a", Format: "pst"}, ErrUsage},
	}
	for _, tt := range tests {
		if _, err := Call(t.Context(), env, Delete, ImportRun, tt.in); !errors.Is(err, tt.want) {
			t.Errorf("Import(%+v) = %v, want %v", tt.in, err, tt.want)
		}
	}
	if _, err := Call(t.Context(), env, Read, ImportRun, ImportIn{}); !errors.Is(err, ErrPermission) {
		t.Errorf("read-only caller: %v", err)
	}
}

func TestImportPartialFailure(t *testing.T) {
	env := newTestEnv(t)
	root := thunderbirdExport(t)
	bad := filepath.Join(root, "Inbox", "cur", "5:2,")
	writeFile(t, bad, mail("5@x", "unreadable"))
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(bad); err == nil {
		_ = f.Close()
		t.Skip("running with privileges that ignore file permissions")
	}
	out, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: root, Account: "personal"})
	if !errors.Is(err, ErrPartial) || out.Imported != 3 || out.Failed != 1 || len(out.Failures) != 1 {
		t.Fatalf("import = %+v, %v", out, err)
	}
}

func TestReindexRestoresTags(t *testing.T) {
	env := newTestEnv(t)
	if _, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: thunderbirdExport(t), Account: "personal", Inbox: true}); err != nil {
		t.Fatal(err)
	}
	before := search(t, env, "tag:inbox tag:folder:inbox")

	out, err := Call(t.Context(), env, Delete, IndexRebuild, struct{}{})
	if err != nil || out.Messages != 3 || out.Accounts != 1 || out.Restored.Applied != 3 {
		t.Fatalf("reindex = %+v, %v", out, err)
	}
	if after := search(t, env, "tag:inbox tag:folder:inbox"); !slices.Equal(before, after) || len(after) != 2 {
		t.Errorf("tags after reindex = %v, want %v", after, before)
	}
	if got := search(t, env, "is:unread"); !slices.Equal(got, []string{"unread two"}) {
		t.Errorf("unread after reindex = %v", got)
	}
}

func TestReindexInterruptedKeepsOldIndexAndTags(t *testing.T) {
	env := newTestEnv(t)
	if _, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: thunderbirdExport(t), Account: "personal", Inbox: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Call(ctx, env, Delete, IndexRebuild, struct{}{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reindex = %v", err)
	}
	if _, err := os.Stat(env.Paths.IndexFile + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Error("partial index left behind")
	}
	// The old index is intact, and a second reindex keeps every tag.
	if got := search(t, env, "tag:inbox"); len(got) != 3 {
		t.Fatalf("inbox after interrupted reindex = %v", got)
	}
	if _, err := Call(t.Context(), env, Delete, IndexRebuild, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if got := search(t, env, "tag:inbox"); len(got) != 3 {
		t.Errorf("inbox after second reindex = %v", got)
	}
}

func TestReindexStopsWhenDumpCannotBeWritten(t *testing.T) {
	env := newTestEnv(t)
	if _, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: thunderbirdExport(t), Account: "personal"}); err != nil {
		t.Fatal(err)
	}
	// Make the dump path unwritable: a directory in its place.
	if err := os.Remove(env.Paths.TagsDump); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(env.Paths.TagsDump, "block"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Delete, IndexRebuild, struct{}{}); err == nil {
		t.Fatal("reindex continued without a fresh dump")
	}
	if got := search(t, env, "tag:folder:inbox"); len(got) != 2 {
		t.Errorf("index changed: %v", got)
	}
}

func TestReindexWithoutDumpUsesFlags(t *testing.T) {
	env := newTestEnv(t)
	if _, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: thunderbirdExport(t), Account: "personal"}); err != nil {
		t.Fatal(err)
	}
	if err := env.closeIndex(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{env.Paths.TagsDump, env.Paths.IndexFile} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	out, err := Call(t.Context(), env, Delete, IndexRebuild, struct{}{})
	if err != nil || out.Messages != 3 {
		t.Fatalf("reindex = %+v, %v", out, err)
	}
	if got := search(t, env, "is:unread"); !slices.Equal(got, []string{"unread two"}) {
		t.Errorf("unread from flags = %v", got)
	}
}

func TestTagsDumpAndRestore(t *testing.T) {
	env := newTestEnv(t)
	if _, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: thunderbirdExport(t), Account: "personal"}); err != nil {
		t.Fatal(err)
	}
	list, err := Call(t.Context(), env, Read, TagsList, struct{}{})
	if err != nil || len(list) != 3 {
		t.Fatalf("tags = %+v, %v", list, err)
	}
	dump, err := Call(t.Context(), env, Read, TagsDump, TagsDumpIn{})
	if err != nil || !strings.Contains(dump.Dump, "+folder:inbox +unread -- id:2@x") {
		t.Fatalf("dump = %q, %v", dump.Dump, err)
	}
	// Mark message 2 read via a restore; its file flags follow.
	edited := strings.Replace(dump.Dump, "+folder:inbox +unread -- id:2@x", "+folder:inbox +todo -- id:2@x", 1)
	file := filepath.Join(t.TempDir(), "edited.dump")
	writeFile(t, file, edited)
	res, err := Call(t.Context(), env, Delete, TagsRestore, TagsRestoreIn{Path: file})
	if err != nil || res.Applied != 3 {
		t.Fatalf("restore = %+v, %v", res, err)
	}
	if got := search(t, env, "is:unread"); len(got) != 0 {
		t.Errorf("still unread: %v", got)
	}
	st, _ := env.Store("personal")
	for e := range st.All() {
		if !strings.Contains(e.Flags, "S") {
			t.Errorf("file %s flags %q, want S", e.Hash[:8], e.Flags)
		}
	}

	out, err := Call(t.Context(), env, Read, TagsDump, TagsDumpIn{Path: filepath.Join(t.TempDir(), "out.dump")})
	if err != nil || out.Path == "" {
		t.Fatalf("dump to file = %+v, %v", out, err)
	}
	if _, err := Call(t.Context(), env, Delete, TagsRestore, TagsRestoreIn{Path: "/nonexistent"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("restore missing = %v", err)
	}
	writeFile(t, file, "garbage line\n")
	if _, err := Call(t.Context(), env, Delete, TagsRestore, TagsRestoreIn{Path: file}); !errors.Is(err, ErrUsage) {
		t.Errorf("restore garbage = %v", err)
	}
}

func TestFolderTag(t *testing.T) {
	for in, want := range map[string]string{
		"INBOX":            "folder:inbox",
		"Sent Items":       "folder:sent-items",
		"  Work / Q4  ":    "folder:work-/-q4",
		"[Gmail]/All Mail": "folder:[gmail]/all-mail",
		"":                 "folder:unnamed",
		"tab\there":        "folder:tab-here",
	} {
		if got := folderTag(in); got != want {
			t.Errorf("folderTag(%q) = %q, want %q", in, got, want)
		}
	}
	if got := folderTag(strings.Repeat("é", 200)); len(got) > 128 {
		t.Errorf("folderTag too long: %d", len(got))
	}
}

func TestTagFlags(t *testing.T) {
	tests := []struct {
		current string
		tags    []string
		want    string
	}{
		{"", []string{"unread"}, ""},
		{"", nil, "S"},
		{"S", []string{"flagged", "replied"}, "SFR"},
		{"DS", []string{"unread"}, "D"},
		{"PT", []string{"flagged"}, "PTSF"},
	}
	for _, tt := range tests {
		if got := tagFlags(tt.current, tt.tags); got != tt.want {
			t.Errorf("tagFlags(%q, %v) = %q, want %q", tt.current, tt.tags, got, tt.want)
		}
	}
}
