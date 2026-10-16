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
