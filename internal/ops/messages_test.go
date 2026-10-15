// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"nought/internal/index"
)

func importSample(t *testing.T, env *Env) {
	t.Helper()
	if _, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: thunderbirdExport(t), Account: "personal", Inbox: true}); err != nil {
		t.Fatal(err)
	}
}

func idOf(t *testing.T, env *Env, q string) string {
	t.Helper()
	out, err := Call(t.Context(), env, Read, MsgSearch, SearchIn{Query: q})
	if err != nil || len(out.Messages) != 1 {
		t.Fatalf("search %q = %+v, %v", q, out, err)
	}
	return out.Messages[0].ID
}

func TestMsgSearch(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	out, err := Call(t.Context(), env, Read, MsgSearch, SearchIn{Query: "tag:inbox", Limit: 2})
	if err != nil || len(out.Messages) != 2 || !out.More {
		t.Fatalf("page 1 = %+v, %v", out, err)
	}
	m := out.Messages[0]
	if len(m.ID) != IDLen || m.Account != "personal" || m.Date == nil || m.From != "alice@example.org" {
		t.Errorf("message = %+v", m)
	}
	out, err = Call(t.Context(), env, Read, MsgSearch, SearchIn{Query: "tag:inbox", Limit: 2, Offset: 2})
	if err != nil || len(out.Messages) != 1 || out.More {
		t.Fatalf("page 2 = %+v, %v", out, err)
	}
	for _, bad := range []SearchIn{{Query: "bogus:x"}, {Limit: 1 << 30}, {Offset: -1}} {
		if _, err := Call(t.Context(), env, Read, MsgSearch, bad); !errors.Is(err, ErrUsage) {
			t.Errorf("search %+v = %v", bad, err)
		}
	}
}

func TestMsgShow(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	id := idOf(t, env, "subject:\"unread two\"")
	out, err := Call(t.Context(), env, Read, MsgShow, ShowIn{ID: id[:8], Raw: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Message.Subject != "unread two" || strings.TrimSpace(out.Body) != "hello unread two" ||
		out.BodyFrom != "text/plain" || !strings.Contains(out.Raw, "Message-ID: <2@x>") {
		t.Errorf("show = %+v", out)
	}
	if !slices.Contains(out.Message.Tags, TagUnread) {
		t.Error("show marked the message read")
	}
	if _, err := Call(t.Context(), env, Read, MsgShow, ShowIn{ID: "ffffff"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing id = %v", err)
	}
	if _, err := Call(t.Context(), env, Read, MsgShow, ShowIn{ID: "xyz"}); !errors.Is(err, ErrUsage) {
		t.Errorf("bad id = %v", err)
	}
}

func TestMsgShowHTMLAndAmbiguity(t *testing.T) {
	env := newTestEnv(t)
	root := filepath.Join(t.TempDir(), "x")
	html := "From: a@x\r\nMessage-ID: <h@x>\r\nContent-Type: text/html\r\n\r\n<p>Hi <img src=\"https://tracker/p.gif\"><script>x()</script></p>\r\n"
	writeFile(t, filepath.Join(root, "a.eml"), html)
	for _, acct := range []string{"one", "two"} {
		if _, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: root, Account: acct}); err != nil {
			t.Fatal(err)
		}
	}
	out, err := Call(t.Context(), env, Read, MsgSearch, SearchIn{Query: "account:one"})
	if err != nil {
		t.Fatal(err)
	}
	id := out.Messages[0].ID
	if _, err := Call(t.Context(), env, Read, MsgShow, ShowIn{ID: id}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("same message in two accounts = %v", err)
	}
	show, err := Call(t.Context(), env, Read, MsgShow, ShowIn{ID: id, Account: "two"})
	if err != nil || show.Body != "Hi" || show.BodyFrom != "text/html" {
		t.Errorf("html show = %+v, %v", show, err)
	}
}

func TestMsgShowMissingFile(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	id := idOf(t, env, "subject:\"read one\"")
	st, _ := env.Store("personal")
	for e := range st.All() {
		if strings.HasPrefix(e.Hash, id) {
			if err := removeFile(env, "personal", e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := Call(t.Context(), env, Read, MsgShow, ShowIn{ID: id}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file = %v", err)
	}
}

func TestMsgTagAndArchive(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)

	dry, err := Call(t.Context(), env, Delete, MsgArchive, ArchiveIn{Query: "tag:inbox", DryRun: true})
	if err != nil || dry.Matched != 3 || dry.Changed != 3 {
		t.Fatalf("dry archive = %+v, %v", dry, err)
	}
	if got := search(t, env, "tag:inbox"); len(got) != 3 {
		t.Fatalf("dry run changed tags: %v", got)
	}

	id := idOf(t, env, "subject:\"unread two\"")
	out, err := Call(t.Context(), env, Delete, MsgTag, TagIn{Query: id, Add: []string{"todo"}, Remove: []string{"unread"}})
	if err != nil || out.Matched != 1 || out.Changed != 1 {
		t.Fatalf("tag = %+v, %v", out, err)
	}
	st, _ := env.Store("personal")
	for e := range st.All() {
		if strings.HasPrefix(e.Hash, id) && !strings.Contains(e.Flags, "S") {
			t.Errorf("file not marked seen: %q", e.Flags)
		}
	}
	again, err := Call(t.Context(), env, Delete, MsgTag, TagIn{Query: id, Add: []string{"todo"}})
	if err != nil || again.Changed != 0 {
		t.Errorf("idempotent tag = %+v, %v", again, err)
	}

	arch, err := Call(t.Context(), env, Delete, MsgArchive, ArchiveIn{Query: "tag:inbox"})
	if err != nil || arch.Changed != 3 {
		t.Fatalf("archive = %+v, %v", arch, err)
	}
	if got := search(t, env, "tag:inbox"); len(got) != 0 {
		t.Errorf("inbox not empty: %v", got)
	}
	if got := search(t, env, "tag:todo"); !slices.Equal(got, []string{"unread two"}) {
		t.Errorf("todo = %v", got)
	}

	for _, bad := range []TagIn{
		{Add: []string{"x"}},
		{Query: "   ", Add: []string{"x"}},
		{Query: "tag:x", Account: "personal", Add: []string{"x"}},
		{Query: "tag:x"},
		{Query: "tag:x", Add: []string{"Bad Tag"}},
		{Query: "bogus:x", Add: []string{"x"}},
	} {
		if _, err := Call(t.Context(), env, Delete, MsgTag, bad); !errors.Is(err, ErrUsage) {
			t.Errorf("tag %+v = %v", bad, err)
		}
	}
	if _, err := Call(t.Context(), env, Read, MsgArchive, ArchiveIn{Query: "x"}); !errors.Is(err, ErrPermission) {
		t.Errorf("read-only archive = %v", err)
	}
}

func TestBareIDMustBeUnique(t *testing.T) {
	env := newTestEnv(t)
	root := filepath.Join(t.TempDir(), "x")
	writeFile(t, filepath.Join(root, "a.eml"), mail("1@x", "in two accounts"))
	for _, acct := range []string{"one", "two"} {
		if _, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: root, Account: acct, Inbox: true}); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := Call(t.Context(), env, Read, MsgSearch, SearchIn{Query: "account:one"})
	id := out.Messages[0].ID
	if _, err := Call(t.Context(), env, Delete, MsgArchive, ArchiveIn{Query: id}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous bare id = %v", err)
	}
	res, err := Call(t.Context(), env, Delete, MsgArchive, ArchiveIn{Query: id, Account: "two"})
	if err != nil || res.Changed != 1 {
		t.Fatalf("archive in one account = %+v, %v", res, err)
	}
	if got := search(t, env, "tag:inbox account:one"); len(got) != 1 {
		t.Errorf("other account touched: %v", got)
	}
	// A word that looks like hex is not silently treated as a search.
	if _, err := Call(t.Context(), env, Delete, MsgArchive, ArchiveIn{Query: "decade"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("hex-looking word = %v", err)
	}
}

func TestSearchMoreAtMaxLimit(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	out, err := Call(t.Context(), env, Read, MsgSearch, SearchIn{Limit: index.MaxLimit})
	if err != nil || out.More || len(out.Messages) != 3 {
		t.Fatalf("search = %d messages, more %v, %v", len(out.Messages), out.More, err)
	}
}
