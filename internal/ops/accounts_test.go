// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/imapsync"
	"github.com/pluja/nought/internal/secret"
)

func addAccount(t *testing.T, env *Env, in AccountIn) AccountOut {
	t.Helper()
	out, err := Call(t.Context(), env, Delete, AccountAdd, in)
	if err != nil {
		t.Fatalf("add %s: %v", in.Name, err)
	}
	return out
}

func TestAccountLifecycle(t *testing.T) {
	env := newTestEnv(t)
	out := addAccount(t, env, AccountIn{Name: "personal", Email: "me@example.org", IMAPHost: "imap.example.org", Password: "hunter2"})
	if out.IMAP != (config.Server{Host: "imap.example.org", Port: 993, TLS: "implicit"}) || out.Deletion != "disabled" ||
		out.PasswordSource != "keyring" || out.Username != "me@example.org" || len(out.Folders) != 1 || out.SMTP != nil {
		t.Errorf("added = %+v", out)
	}
	if pw, _ := env.Secrets.Get("personal"); pw != "hunter2" {
		t.Errorf("keyring has %q", pw)
	}
	cfgText, _ := os.ReadFile(env.Paths.ConfigFile)
	if strings.Contains(string(cfgText), "hunter2") {
		t.Fatal("password written to the config file")
	}

	work := addAccount(t, env, AccountIn{Name: "work", Email: "me@corp.example", IMAPHost: "mail.corp.example",
		IMAPTLS: "starttls", SMTPHost: "mail.corp.example", SMTPTLS: "starttls", PasswordCommand: "pass show work"})
	if work.IMAP.Port != 143 || work.SMTP == nil || work.SMTP.Port != 587 || work.PasswordSource != "command" {
		t.Errorf("work = %+v", work)
	}

	list, err := Call(t.Context(), env, Read, AccountList, struct{}{})
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}

	set, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "personal", Deletion: "enabled", Color: "#112233"})
	if err != nil || set.Deletion != "enabled" || set.Color != "#112233" {
		t.Fatalf("set = %+v, %v", set, err)
	}
	reloaded, err := config.Load(env.Paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := reloaded.Account("personal"); !a.DeletionEnabled() {
		t.Error("deletion setting not saved")
	}

	rm, err := Call(t.Context(), env, Delete, AccountRemove, AccountName{Name: "personal"})
	if err != nil || rm.Archive != env.Paths.AccountArchive("personal") {
		t.Fatalf("remove = %+v, %v", rm, err)
	}
	if _, err := env.Secrets.Get("personal"); !errors.Is(err, secret.ErrNotFound) {
		t.Error("password left in keyring")
	}
	if _, err := Call(t.Context(), env, Read, AccountShow, AccountName{Name: "personal"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("show removed = %v", err)
	}
	show, err := Call(t.Context(), env, Read, AccountShow, AccountName{Name: "work"})
	if err != nil || show.Name != "work" {
		t.Errorf("show = %+v, %v", show, err)
	}
}

func TestAccountAddErrors(t *testing.T) {
	env := newTestEnv(t)
	addAccount(t, env, AccountIn{Name: "a", Email: "a@x.org", IMAPHost: "imap.x.org", Password: "p"})
	tests := []AccountIn{
		{Name: "a", Email: "a@x.org", IMAPHost: "imap.x.org", Password: "p"},                       // duplicate
		{Name: "b", Email: "b@x.org", IMAPHost: "imap.x.org"},                                      // no password
		{Name: "b", Email: "b@x.org", IMAPHost: "imap.x.org", Password: "p", PasswordCommand: "x"}, // both
		{Name: "B!", Email: "b@x.org", IMAPHost: "imap.x.org", Password: "p"},                      // bad name
		{Name: "b", Email: "not-an-address", IMAPHost: "imap.x.org", Password: "p"},                // bad email
		{Name: "b", Email: "b@x.org", IMAPHost: "imap.x.org", IMAPTLS: "none", Password: "p"},      // plaintext
		{Name: "b", Email: "b@x.org", IMAPHost: "", Password: "p"},                                 // no host
	}
	for _, in := range tests {
		if _, err := Call(t.Context(), env, Delete, AccountAdd, in); !errors.Is(err, ErrUsage) {
			t.Errorf("add %+v = %v, want usage error", in, err)
		}
	}
	if _, err := env.Secrets.Get("b"); !errors.Is(err, secret.ErrNotFound) {
		t.Error("failed add left a password behind")
	}
	if _, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "a"}); !errors.Is(err, ErrUsage) {
		t.Errorf("empty set = %v", err)
	}
	if _, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "a", Deletion: "maybe"}); !errors.Is(err, ErrUsage) {
		t.Errorf("bad deletion = %v", err)
	}
	if _, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "zz", Label: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("set missing = %v", err)
	}
	if _, err := Call(t.Context(), env, Delete, AccountRemove, AccountName{Name: "zz"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("remove missing = %v", err)
	}
	if _, err := Call(t.Context(), env, Tag, AccountSet, AccountSetIn{Name: "a", Deletion: "enabled"}); !errors.Is(err, ErrPermission) {
		t.Errorf("tag-level caller enabled deletion: %v", err)
	}
}

func TestDefaultFolderRulesIgnoreWorkInProgress(t *testing.T) {
	a := config.Account{Folders: DefaultFolderRules()}
	for _, f := range []string{"Drafts", "Templates", "Junk", "Spam", "Trash", "Trash/Old", "Deleted Messages",
		"Deleted Items", "[Gmail]/Drafts", "[Gmail]/Spam", "INBOX/Drafts", "INBOX/Trash", "DRAFTS", "Junk E-mail"} {
		if p := a.Policy(f); p.Action != config.ActionIgnore {
			t.Errorf("%s: %+v, want ignore", f, p)
		}
	}
	for _, f := range []string{"INBOX", "Sent", "Archive", "Sent Items", "Projects/Robinson"} {
		if p := a.Policy(f); p.Action != config.ActionArchive {
			t.Errorf("%s: %+v, want archive", f, p)
		}
	}
}

func TestAccountChangesKeepTheRestOfTheConfig(t *testing.T) {
	env := newTestEnv(t)
	addAccount(t, env, AccountIn{Name: "a", Email: "a@x.org", IMAPHost: "imap.x.org", Password: "p"})
	cfg, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	next, seven := *cfg, 7
	next.TrashDays, next.SendDelay, next.PreferBody = &seven, "30s", config.BodyPlain
	if err := saveConfig(env, &next); err != nil {
		t.Fatal(err)
	}
	steps := map[string]func() error{
		"add": func() error {
			_, err := Call(t.Context(), env, Delete, AccountAdd, AccountIn{Name: "b", Email: "b@x.org", IMAPHost: "imap.x.org", Password: "p"})
			return err
		},
		"set": func() error {
			_, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "a", Label: "A"})
			return err
		},
		"remove": func() error {
			_, err := Call(t.Context(), env, Delete, AccountRemove, AccountName{Name: "b"})
			return err
		},
	}
	for _, name := range []string{"add", "set", "remove"} {
		if err := steps[name](); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := config.Load(env.Paths.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		if got.TrashDays == nil || *got.TrashDays != 7 || got.SendDelay != "30s" || got.PreferBody != config.BodyPlain {
			t.Errorf("account %s dropped settings: %+v", name, got)
		}
	}
}

func TestAccountSetSMTP(t *testing.T) {
	env := newTestEnv(t)
	addAccount(t, env, AccountIn{Name: "a", Email: "a@x.org", IMAPHost: "imap.x.org", Password: "p"})
	out, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "a", SMTPHost: "smtp.x.org", SMTPTLS: config.TLSStartTLS})
	if err != nil || out.SMTP == nil || *out.SMTP != (config.Server{Host: "smtp.x.org", Port: 587, TLS: config.TLSStartTLS}) {
		t.Fatalf("set smtp = %+v, %v", out.SMTP, err)
	}
	if _, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "a", SMTPHost: "smtp.x.org", SMTPTLS: "none"}); !errors.Is(err, ErrUsage) {
		t.Errorf("plaintext smtp = %v", err)
	}
	if _, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "a", SaveSent: config.SaveSentServer}); err != nil {
		t.Fatal(err)
	}
	if a, err := account(env, "a"); err != nil || a.SaveSent != config.SaveSentServer {
		t.Errorf("save_sent not set: %+v, %v", a, err)
	}
	if _, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "a", SaveSent: "cloud"}); !errors.Is(err, ErrUsage) {
		t.Errorf("bad save_sent = %v", err)
	}
}

// Review: folder names are localized (Entwürfe, Papierkorb, Corbeille...),
// so the folders a server marks as drafts, junk, trash, all mail or flagged
// are left alone unless a rule names them. Gmail's All Mail holds the inbox
// too: archiving it, then deleting there, would delete the inbox.
func TestSpecialFoldersAreIgnoredWhateverTheirName(t *testing.T) {
	a := &config.Account{Folders: append(DefaultFolderRules(), config.FolderRule{Match: "Archivo", Action: config.ActionArchive})}
	tests := []struct {
		f    imapsync.Folder
		want string
	}{
		{imapsync.Folder{Name: "Entwürfe", Special: true}, config.ActionIgnore},
		{imapsync.Folder{Name: "Papierkorb", Trash: true}, config.ActionIgnore},
		{imapsync.Folder{Name: "[Gmail]/Alle Nachrichten", Special: true}, config.ActionIgnore},
		{imapsync.Folder{Name: "Gesendet", Sent: true}, config.ActionArchive},
		{imapsync.Folder{Name: "Rechnungen"}, config.ActionArchive},
		// A rule that names the folder wins.
		{imapsync.Folder{Name: "Archivo", Special: true}, config.ActionArchive},
	}
	for _, tt := range tests {
		if got := folderPolicy(a, tt.f).Action; got != tt.want {
			t.Errorf("%+v: %s, want %s", tt.f, got, tt.want)
		}
	}
}

// Review: --json is a stable interface: snake_case names everywhere, and
// lists as [] (never null).
func TestJSONSchemaIsConsistent(t *testing.T) {
	env := draftEnv(t)
	acct, err := Call(t.Context(), env, Read, AccountShow, AccountName{Name: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := Call(t.Context(), env, Draft, DraftNew, DraftNewIn{Subject: "s", Body: "x\n"})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := Call(t.Context(), env, Draft, DraftNew, DraftNewIn{To: []string{"a@x.org"}, Subject: "s", Body: "x\n"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{acct, d, ok} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if regexp.MustCompile(`"[A-Z][A-Za-z]*":`).MatchString(s) || strings.Contains(s, ":null") {
			t.Errorf("not snake_case or has null:\n%s", s)
		}
	}
}

// Review: small CLI edges.
func TestSmallEdges(t *testing.T) {
	env := newTestEnv(t)
	// No account yet: say so, not "several accounts".
	if _, err := Call(t.Context(), env, Draft, DraftNew, DraftNewIn{}); err == nil || !strings.Contains(err.Error(), "no account") {
		t.Errorf("no account: %v", err)
	}
	addAccount(t, env, AccountIn{Name: "a", Email: "a@x.org", IMAPHost: "imap.x.org", Password: "p", SMTPHost: "smtp.x.org"})
	// --smtp-port alone changes the port of the existing server.
	out, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "a", SMTPPort: 2525})
	if err != nil || out.SMTP == nil || *out.SMTP != (config.Server{Host: "smtp.x.org", Port: 2525, TLS: config.TLSImplicit}) {
		t.Errorf("smtp port alone: %+v, %v", out.SMTP, err)
	}
	// A negative limit is a mistake, not "the default".
	if _, err := Call(t.Context(), env, Read, MsgSearch, SearchIn{Query: "tag:inbox", Limit: -5}); !errors.Is(err, ErrUsage) {
		t.Errorf("negative limit: %v", err)
	}
}

// Review: config.toml holds password_command, which nou runs: a file others
// can write is a way to run code as you, so nou warns about it.
func TestWarnsAboutAWritableConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	env := newTestEnv(t)
	addAccount(t, env, AccountIn{Name: "a", Email: "a@x.org", IMAPHost: "imap.x.org", Password: "p"})
	if err := os.Chmod(env.Paths.ConfigFile, 0o666); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	fresh := NewEnv(env.Paths, slog.New(slog.NewTextHandler(&log, nil)), env.Now, BuildInfo{}, env.Secrets)
	if _, err := fresh.Config(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "others can change") {
		t.Errorf("no warning: %q", log.String())
	}
}
