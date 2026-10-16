// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/pluja/nought/internal/config"
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
