// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pluja/nought/internal/compose"
)

// draftEnv is a test env with an account "personal" (me@example.org) and
// the sample mail imported.
func draftEnv(t *testing.T) *Env {
	t.Helper()
	env := newTestEnv(t)
	addAccount(t, env, AccountIn{Name: "personal", Email: "me@example.org", IMAPHost: "imap.example.org", IMAPPort: 993,
		Password: "x"})
	importSample(t, env)
	return env
}

func TestDraftReplyLifecycle(t *testing.T) {
	env := draftEnv(t)
	id := idOf(t, env, `subject:"read one"`)
	out, err := Call(t.Context(), env, Draft, DraftReply, DraftReplyIn{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(out.Draft.To, []string{"alice@example.org"}) || out.Draft.Subject != "Re: read one" ||
		out.InReplyTo != "1@x" || !strings.Contains(out.Draft.Body, "> hello read one") || out.Draft.From != "me@example.org" {
		t.Errorf("reply draft = %+v", out)
	}
	info, err := os.Stat(out.Path)
	if err != nil || info.Mode().Perm() != 0o600 || !strings.HasPrefix(out.Path, env.Paths.DraftsDir) {
		t.Fatalf("editor file %s: %v %v", out.Path, info, err)
	}
	// The user edits the file.
	text, _ := os.ReadFile(out.Path)
	edited := strings.Replace(string(text), "\n\n", "\n\nSee you there.", 1)
	if err := os.WriteFile(out.Path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	up, err := Call(t.Context(), env, Draft, DraftUpdate, DraftIDIn{ID: out.ID})
	if err != nil || !strings.HasPrefix(up.Draft.Body, "See you there.") || up.InReplyTo != "1@x" {
		t.Errorf("update = %+v, %v", up, err)
	}
	list, _ := Call(t.Context(), env, Read, DraftList, struct{}{})
	if len(list) != 1 || list[0].ID != out.ID || list[0].Subject != "Re: read one" {
		t.Errorf("list = %+v", list)
	}
	if _, err := Call(t.Context(), env, Draft, DraftDiscard, DraftIDIn{ID: out.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out.Path); !errors.Is(err, os.ErrNotExist) {
		t.Error("discard left the file")
	}
	if _, err := Call(t.Context(), env, Read, DraftShow, DraftIDIn{ID: "../../etc/passwd"}); !errors.Is(err, ErrUsage) {
		t.Errorf("a bad draft id: %v", err)
	}
}

func TestDraftNewAndForward(t *testing.T) {
	env := draftEnv(t)
	n, err := Call(t.Context(), env, Draft, DraftNew, DraftNewIn{To: []string{"bob@example.org"}, Subject: "Hi", Body: "Hello\n"})
	if err != nil || n.Draft.Subject != "Hi" || n.Draft.Body != "Hello\n" || n.Kind != "new" {
		t.Errorf("new = %+v, %v", n, err)
	}
	if slices.ContainsFunc(n.Problems, func(p compose.Problem) bool { return p.Blocking }) {
		t.Errorf("new draft has blocking problems: %+v", n.Problems)
	}
	f, err := Call(t.Context(), env, Draft, DraftForward, DraftForwardIn{ID: idOf(t, env, `subject:"read one"`)})
	if err != nil || f.Draft.Subject != "Fwd: read one" || f.Kind != "forward" || f.SourceHash == "" {
		t.Errorf("forward = %+v, %v", f, err)
	}
}

func TestReplyToHTMLOnlyQuotesText(t *testing.T) { // Review Focus 2
	env := draftEnv(t)
	root := filepath.Join(t.TempDir(), "export")
	writeFile(t, filepath.Join(root, "Inbox", "cur", maildirName("1", "S")), "From: a@example.org\r\nMessage-ID: <h@x>\r\n"+
		"Subject: html\r\nContent-Type: text/html\r\n\r\n<p>Hello <b>there</b></p><script>x()</script>\r\n")
	if _, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: root, Account: "personal"}); err != nil {
		t.Fatal(err)
	}
	out, err := Call(t.Context(), env, Draft, DraftReply, DraftReplyIn{ID: idOf(t, env, "subject:html")})
	if err != nil || !strings.Contains(out.Draft.Body, "> Hello there") || strings.Contains(out.Draft.Body, "<") {
		t.Errorf("quote = %q, %v", out.Draft.Body, err)
	}
}

func TestDraftsNeedDraftPermission(t *testing.T) {
	env := draftEnv(t)
	if _, err := Call(t.Context(), env, Tag, DraftNew, DraftNewIn{}); !errors.Is(err, ErrPermission) {
		t.Errorf("tag-level draft: %v", err)
	}
}

func TestAddressSuggest(t *testing.T) {
	env := draftEnv(t)
	got, err := Call(t.Context(), env, Read, AddressSuggest, SuggestIn{Prefix: "ALI"})
	if err != nil || len(got) == 0 || got[0].Address != "alice@example.org" {
		t.Errorf("suggest = %+v, %v", got, err)
	}
}

// Scripts and agents write the whole message in one call.
func TestDraftsFromScripts(t *testing.T) {
	env := draftEnv(t)
	n, err := Call(t.Context(), env, Draft, DraftNew, DraftNewIn{To: []string{"a@x.org"}, Cc: []string{"c@x.org"}, Bcc: []string{"b@x.org"}, Subject: "s"})
	if err != nil || !slices.Equal(n.Draft.Cc, []string{"c@x.org"}) || !slices.Equal(n.Draft.Bcc, []string{"b@x.org"}) {
		t.Errorf("new = %+v, %v", n.Draft, err)
	}
	id := idOf(t, env, `subject:"read one"`)
	r, err := Call(t.Context(), env, Draft, DraftReply, DraftReplyIn{ID: id, Body: "Sounds good.\n"})
	if err != nil || !strings.HasPrefix(r.Draft.Body, "Sounds good.\n\n") || !strings.Contains(r.Draft.Body, "> hello read one") {
		t.Errorf("reply body = %q, %v", r.Draft.Body, err)
	}
	f, err := Call(t.Context(), env, Draft, DraftForward, DraftForwardIn{ID: id, To: []string{"bob@x.org"}, Body: "FYI"})
	if err != nil || !slices.Equal(f.Draft.To, []string{"bob@x.org"}) || !strings.HasPrefix(f.Draft.Body, "FYI\n\n") ||
		!strings.Contains(f.Draft.Body, "hello read one") {
		t.Errorf("forward = %+v, %v", f.Draft, err)
	}
	if got, err := Call(t.Context(), env, Read, DraftShow, DraftIDIn{ID: f.ID}); err != nil || !slices.Equal(got.Draft.To, f.Draft.To) {
		t.Errorf("forward recipients not saved: %+v, %v", got.Draft, err)
	}
}

// Frontends change recipients and attachments without editing the file.
func TestDraftSet(t *testing.T) {
	env := draftEnv(t)
	d, err := Call(t.Context(), env, Draft, DraftNew, DraftNewIn{Subject: "s", Body: "Hello\n"})
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := Call(t.Context(), env, Draft, DraftSet, DraftSetIn{ID: d.ID, To: `bob@x.org, "Doe, Jane" <jane@x.org>`, Attach: []string{file}})
	if err != nil || !slices.Equal(out.Draft.To, []string{"bob@x.org", `"Doe, Jane" <jane@x.org>`}) || !slices.Equal(out.Draft.Attach, []string{file}) ||
		!strings.HasPrefix(out.Draft.Body, "Hello\n") || len(out.To) != 2 {
		t.Fatalf("set = %+v, %v", out, err)
	}
	again, err := Call(t.Context(), env, Read, DraftShow, DraftIDIn{ID: d.ID})
	if err != nil || !slices.Equal(again.Draft.Attach, []string{file}) || again.Draft.Body != out.Draft.Body {
		t.Errorf("not saved: %+v, %v", again.Draft, err)
	}
	if _, err := Call(t.Context(), env, Tag, DraftSet, DraftSetIn{ID: d.ID}); !errors.Is(err, ErrPermission) {
		t.Errorf("tag-level set = %v", err)
	}
}

// Quitting the editor without saving drops a draft nothing was written in.
func TestDraftUpdateDiscardsUntouchedDraft(t *testing.T) {
	env := draftEnv(t)
	n, err := Call(t.Context(), env, Draft, DraftNew, DraftNewIn{})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := Call(t.Context(), env, Draft, DraftUpdate, DraftIDIn{ID: n.ID}); err != nil || !out.Discarded {
		t.Errorf("untouched new draft: %+v, %v", out, err)
	}
	r, err := Call(t.Context(), env, Draft, DraftReply, DraftReplyIn{ID: idOf(t, env, `subject:"read one"`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.Path, []byte(strings.Replace(compose.Render(r.Draft), "\n\n", "\n\nOK.", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 { // written once: kept, even when later left untouched
		if out, err := Call(t.Context(), env, Draft, DraftUpdate, DraftIDIn{ID: r.ID}); err != nil || out.Discarded {
			t.Fatalf("edited reply: %+v, %v", out, err)
		}
	}
	if list, _ := Call(t.Context(), env, Read, DraftList, struct{}{}); len(list) != 1 || list[0].ID != r.ID {
		t.Errorf("drafts = %+v", list)
	}
}

// Review: text given up front (a script, --body-file) is never dropped as
// "untouched".
func TestDraftWithGivenTextIsKept(t *testing.T) {
	env := draftEnv(t)
	inputs := []func() (DraftOut, error){
		func() (DraftOut, error) {
			return Call(t.Context(), env, Draft, DraftNew, DraftNewIn{Body: "Dear Bob\n"})
		},
		func() (DraftOut, error) {
			return Call(t.Context(), env, Draft, DraftNew, DraftNewIn{To: []string{"bob@x.org"}})
		},
		func() (DraftOut, error) {
			return Call(t.Context(), env, Draft, DraftReply, DraftReplyIn{ID: idOf(t, env, `subject:"read one"`), Body: "OK"})
		},
		func() (DraftOut, error) {
			return Call(t.Context(), env, Draft, DraftForward, DraftForwardIn{ID: idOf(t, env, `subject:"read one"`), To: []string{"bob@x.org"}})
		},
	}
	for i, start := range inputs {
		d, err := start()
		if err != nil {
			t.Fatal(err)
		}
		if out, err := Call(t.Context(), env, Draft, DraftUpdate, DraftIDIn{ID: d.ID}); err != nil || out.Discarded {
			t.Errorf("input %d: discarded (%v)", i, err)
		}
	}
}

// Review: a line break in a header value would make a header of its own in
// the editor file (a hidden Bcc).
func TestDraftInputsRefuseControlCharacters(t *testing.T) {
	env := draftEnv(t)
	bad := []DraftNewIn{
		{Subject: "hi\nBcc: evil@x.org"},
		{To: []string{"a@x.org\nBcc: evil@x.org"}},
		{Cc: []string{"a@x.org\r"}},
		{Attach: []string{"/tmp/a\nBcc: evil@x.org"}},
	}
	for _, in := range bad {
		if _, err := Call(t.Context(), env, Draft, DraftNew, in); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v", in, err)
		}
	}
	d, err := Call(t.Context(), env, Draft, DraftNew, DraftNewIn{Subject: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Draft, DraftSet, DraftSetIn{ID: d.ID, To: "a@x.org\nBcc: evil@x.org"}); !errors.Is(err, ErrUsage) {
		t.Errorf("draft.set: %v", err)
	}
	id := idOf(t, env, `subject:"read one"`)
	if _, err := Call(t.Context(), env, Draft, DraftForward, DraftForwardIn{ID: id, To: []string{"b@x.org\nBcc: e@x.org"}}); !errors.Is(err, ErrUsage) {
		t.Errorf("forward: %v", err)
	}
}

// Mail arrives with CRLF line endings: the quote in the editor file must
// not show them (^M in vim), nor invisible characters from a hostile mail.
func TestReplyQuoteIsCleanText(t *testing.T) {
	env := draftEnv(t)
	out, err := Call(t.Context(), env, Draft, DraftReply, DraftReplyIn{ID: idOf(t, env, `subject:"read one"`)})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(out.Path)
	if err != nil || strings.ContainsRune(string(file), '\r') || !strings.Contains(string(file), "> hello read one") {
		t.Errorf("editor file %q, %v", file, err)
	}
}
