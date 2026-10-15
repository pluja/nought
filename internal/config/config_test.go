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
			include, optional := p.BackupSet()
			if len(include) != 4 || len(optional) != 1 || optional[0] != p.IndexFile {
				t.Errorf("BackupSet = %v, %v", include, optional)
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
		{"empty email", replace(`email = "me@example.org"`, `email = ""`), "email"},
		{"bad color", replace(`#44aa99`, `red`), "color"},
		{"long label", replace(`label = "P"`, `label = "much too long"`), "label"},
		{"bad deletion", replace(`deletion = "disabled"`, `deletion = "yes"`), "deletion"},
		{"no host", replace(`host = "imap.example.org"`, `host = ""`), "host"},
		{"bad port", replace(`port = 993`, `port = 0`), "port"},
		{"plaintext", replace(`tls = "implicit"`, `tls = "none"`), "tls"},
		{"bad glob", replace(`match = "INBOX"`, `match = "[INBOX"`), "glob"},
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
		{"Other", Policy{ActionArchive, DefaultKeepDays}},
	}
	for _, tt := range tests {
		if got := a.Policy(tt.folder); got != tt.want {
			t.Errorf("Policy(%q) = %+v, want %+v", tt.folder, got, tt.want)
		}
	}
}

func TestExpandBraces(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"{a,b}", "a b"},
		{"x{a,b}y{1,2}", "xay1 xay2 xby1 xby2"},
		{"open{a", "open{a"},
	}
	for _, tt := range tests {
		if got := strings.Join(expandBraces(tt.in), " "); got != tt.want {
			t.Errorf("expandBraces(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
