// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestResolvePaths(t *testing.T) {
	tests := []struct {
		name       string
		goos       string
		vars       map[string]string
		wantConfig string
		wantData   string
		wantErr    bool
	}{
		{
			name:       "linux defaults",
			goos:       "linux",
			vars:       map[string]string{"HOME": "/home/u"},
			wantConfig: "/home/u/.config/nought/config.toml",
			wantData:   "/home/u/.local/share/nought",
		},
		{
			name:       "linux xdg",
			goos:       "linux",
			vars:       map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "/c", "XDG_DATA_HOME": "/d"},
			wantConfig: "/c/nought/config.toml",
			wantData:   "/d/nought",
		},
		{
			name:       "linux ignores relative xdg",
			goos:       "linux",
			vars:       map[string]string{"HOME": "/home/u", "XDG_DATA_HOME": "rel"},
			wantConfig: "/home/u/.config/nought/config.toml",
			wantData:   "/home/u/.local/share/nought",
		},
		{
			name:       "darwin",
			goos:       "darwin",
			vars:       map[string]string{"HOME": "/Users/u"},
			wantConfig: "/Users/u/Library/Application Support/nought/config.toml",
			wantData:   "/Users/u/Library/Application Support/nought",
		},
		{
			name:       "windows",
			goos:       "windows",
			vars:       map[string]string{"AppData": "/r", "LocalAppData": "/l"},
			wantConfig: "/r/nought/config.toml",
			wantData:   "/l/nought",
		},
		{
			name:       "nought home overrides",
			goos:       "linux",
			vars:       map[string]string{"HOME": "/home/u", "NOUGHT_HOME": "/n"},
			wantConfig: "/n/config/config.toml",
			wantData:   "/n/data",
		},
		{name: "relative nought home", goos: "linux", vars: map[string]string{"NOUGHT_HOME": "n"}, wantErr: true},
		{name: "no home", goos: "linux", vars: map[string]string{}, wantErr: true},
		{name: "darwin no home", goos: "darwin", vars: map[string]string{}, wantErr: true},
		{name: "windows no appdata", goos: "windows", vars: map[string]string{}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ResolvePaths(tt.goos, env(tt.vars))
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := filepath.ToSlash(p.ConfigFile); got != tt.wantConfig {
				t.Errorf("ConfigFile = %q, want %q", got, tt.wantConfig)
			}
			if got := filepath.ToSlash(p.DataDir); got != tt.wantData {
				t.Errorf("DataDir = %q, want %q", got, tt.wantData)
			}
			if got := filepath.ToSlash(p.IndexFile); got != tt.wantData+"/index.db" {
				t.Errorf("IndexFile = %q", got)
			}
			if got := filepath.ToSlash(p.AccountArchive("work")); got != tt.wantData+"/archive/work" {
				t.Errorf("AccountArchive = %q", got)
			}
			include, optional, never := p.BackupSet()
			if len(include) != 4 || len(optional) != 1 || optional[0] != p.IndexFile || len(never) != 1 || never[0] != p.TransientDir {
				t.Errorf("BackupSet = %v, %v, %v", include, optional, never)
			}
		})
	}
}

const validTOML = `
[[account]]
name = "personal"
label = "P"
color = "#44aa99"
email = "me@example.org"
deletion = "disabled"

[account.imap]
host = "imap.example.org"
port = 993
tls = "implicit"

[[account.folder]]
match = "INBOX"
keep_days = 7

[[account.folder]]
match = "{Junk,Spam,Trash}"
action = "ignore"
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Accounts) != 0 {
		t.Errorf("accounts = %v, want none", c.Accounts)
	}
}

func TestLoadUnreadable(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("loading a directory succeeded")
	}
}

func TestLoadSaveRoundTrip(t *testing.T) {
	c, err := Load(writeConfig(t, validTOML))
	if err != nil {
		t.Fatal(err)
	}
	a, ok := c.Account("personal")
	if !ok {
		t.Fatal("account personal not found")
	}
	if a.Login() != "me@example.org" || a.DeletionEnabled() {
		t.Errorf("unexpected account %+v", a)
	}

	path := filepath.Join(t.TempDir(), "out", "config.toml")
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Accounts) != 1 || again.Accounts[0].Folders[1].Action != ActionIgnore {
		t.Errorf("round trip lost data: %+v", again)
	}
	if _, ok := again.Account("missing"); ok {
		t.Error("found a missing account")
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{"unknown key", func(s string) string { return s + "\nbogus = 1\n" }, "bogus"},
		{"bad toml", func(s string) string { return s + "\n[[[\n" }, "parse config"},
		{"bad name", replace(`name = "personal"`, `name = "../evil"`), "name must be"},
		{"reserved name", replace(`name = "personal"`, `name = "nul"`), "name must be"},
		{"bad email", replace(`email = "me@example.org"`, `email = "Me <me@example.org>"`), "email"},
		{"control in username", replace(`email = "me@example.org"`, "email = \"me@example.org\"\nusername = \"a\\u0007\""), "control"},
		{"host with port", replace(`host = "imap.example.org"`, `host = "imap.example.org:993"`), "host"},
		{"huge keep", replace(`keep_days = 7`, `keep_days = 99999`), "keep_days"},
		{"empty email", replace(`email = "me@example.org"`, `email = ""`), "email"},
		{"bad color", replace(`#44aa99`, `red`), "color"},
		{"long label", replace(`label = "P"`, `label = "much too long"`), "label"},
		{"bad deletion", replace(`deletion = "disabled"`, `deletion = "yes"`), "deletion"},
		{"no host", replace(`host = "imap.example.org"`, `host = ""`), "host"},
		{"bad port", replace(`port = 993`, `port = 0`), "port"},
		{"plaintext", replace(`tls = "implicit"`, `tls = "none"`), "tls"},
		{"bad glob", replace(`match = "INBOX"`, `match = "[INBOX"`), "unclosed"},
		{"empty match", replace(`match = "INBOX"`, `match = ""`), "match"},
		{"negative keep", replace(`keep_days = 7`, `keep_days = -1`), "keep_days"},
		{"bad action", replace(`action = "ignore"`, `action = "delete"`), "action"},
		{"duplicate", func(s string) string { return s + s }, "duplicate"},
		{"bad smtp", func(s string) string {
			return s + "\n[account.smtp]\nhost = \"smtp.example.org\"\nport = 465\n"
		}, "smtp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.mutate(validTOML)))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadTooLarge(t *testing.T) {
	_, err := Load(writeConfig(t, validTOML+strings.Repeat("# padding\n", 120000)))
	if err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("err = %v", err)
	}
}

func TestRulesAndTrash(t *testing.T) {
	c, err := Load(writeConfig(t, "trash_days = 7\n"+validTOML+"\n[[rule]]\nquery = 'subject:\"verification code\" OR subject:otp'\nexpire_days = 1\n[[rule]]\nquery = \"from:shop.example\"\ntags = [\"receipts\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TrashKeepDays() != 7 || len(c.Rules) != 2 || c.Rules[0].ExpireDays != 1 || c.Rules[1].Tags[0] != "receipts" {
		t.Errorf("config = %+v", c)
	}
	if (&Config{}).TrashKeepDays() != DefaultTrashDays {
		t.Error("default trash days")
	}
	for _, bad := range []string{
		"\n[[rule]]\nquery = \"bogus:x\"\nexpire_days = 1\n",
		"\n[[rule]]\nquery = \"from:x\"\n",
		"\n[[rule]]\nquery = \"\"\nexpire_days = 1\n",
		"\n[[rule]]\nquery = \"from:x\"\ntags = [\"Bad Tag\"]\n",
		"\n[[rule]]\nquery = \"from:x\"\ntags = [\"deleted\"]\n",
		"\n[[rule]]\nquery = \"from:x\"\ntags = [\"delete-after:2000-01-01\"]\n",
		"\n[[rule]]\nquery = \"from:x\"\ntags = [\"-inbox\"]\n",
		"\n[[rule]]\nquery = \"from:x\"\nexpire_days = -1\n",
	} {
		if _, err := Load(writeConfig(t, validTOML+bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestBadTrashDays(t *testing.T) {
	if _, err := Load(writeConfig(t, "trash_days = -2\n"+validTOML)); err == nil {
		t.Error("accepted negative trash_days")
	}
}

func TestTUISection(t *testing.T) {
	c, err := Load(writeConfig(t, validTOML+"\n[tui]\nascii = true\nmark_read_on_open = false\n[[tui.search]]\nname = \"Work\"\nquery = \"account:work\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.TUI.ASCII || c.TUI.ReadOnOpen() || len(c.TUI.Searches) != 1 || c.TUI.Searches[0].Query != "account:work" {
		t.Errorf("tui = %+v", c.TUI)
	}
	if (TUI{}).ReadOnOpen() != true {
		t.Error("mark read on open should default to true")
	}
	if _, err := Load(writeConfig(t, validTOML+"\n[[tui.search]]\nname = \"\"\nquery = \"x\"\n")); err == nil {
		t.Error("accepted a nameless search")
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	c := &Config{Accounts: []Account{{Name: "BAD"}}}
	if err := Save(filepath.Join(t.TempDir(), "c.toml"), c); err == nil {
		t.Fatal("saved an invalid config")
	}
}

func replace(old, repl string) func(string) string {
	return func(s string) string { return strings.Replace(s, old, repl, 1) }
}

func TestPolicy(t *testing.T) {
	zero, three := 0, 3
	a := Account{Folders: []FolderRule{
		{Match: "INBOX", KeepDays: &three},
		{Match: "Sent*", KeepDays: &zero},
		{Match: "{Junk,Spam,Trash}", Action: ActionIgnore},
		{Match: "Archive/*"},
	}}
	tests := []struct {
		folder string
		want   Policy
	}{
		{"INBOX", Policy{ActionArchive, 3}},
		{"Sent", Policy{ActionArchive, 0}},
		{"Sent Items", Policy{ActionArchive, 0}},
		{"Junk", Policy{ActionIgnore, DefaultKeepDays}},
		{"Trash", Policy{ActionIgnore, DefaultKeepDays}},
		{"Archive/2024", Policy{ActionArchive, DefaultKeepDays}},
		{"Junk/Old", Policy{ActionArchive, DefaultKeepDays}}, // rule is exact {Junk,...}
		{"Sent/2020", Policy{ActionArchive, 0}},
		{"Other", Policy{ActionArchive, DefaultKeepDays}},
	}
	for _, tt := range tests {
		if got := a.Policy(tt.folder); got != tt.want {
			t.Errorf("Policy(%q) = %+v, want %+v", tt.folder, got, tt.want)
		}
	}
}

func TestGlob(t *testing.T) {
	tests := []struct {
		glob, name string
		want       bool
	}{
		{"INBOX", "INBOX", true},
		{"INBOX", "INBOX/sub", false},
		{"Junk*", "Junk", true},
		{"Junk*", "Junk/Old", true}, // "*" crosses "/" on purpose
		{"*", "a/b/c", true},
		{"{Junk,Spam}", "Spam", true},
		{"{Junk,Spam}", "Trash", false},
		{"x{a,b}y{1,2}", "xby2", true},
		{"S?nt", "Sent", true},
		{"[ST]ent", "Tent", true},
		{"[!S]ent", "Sent", false},
		{"a.b", "axb", false},
		{"Entwürfe*", "Entwürfe/alt", true},
		{`star\*`, "star*", true},
		{`star\*`, "starx", false},
	}
	for _, tt := range tests {
		re, err := compileGlob(tt.glob)
		if err != nil {
			t.Errorf("compileGlob(%q): %v", tt.glob, err)
			continue
		}
		if got := re.MatchString(tt.name); got != tt.want {
			t.Errorf("glob %q on %q = %v, want %v", tt.glob, tt.name, got, tt.want)
		}
	}
	for _, bad := range []string{"[abc", "{a,b", "{a,{b}}", `trail\`, "[]", strings.Repeat("a", 300)} {
		if _, err := compileGlob(bad); err == nil {
			t.Errorf("compileGlob(%q) accepted", bad)
		}
	}
}
