// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config loads, validates and saves the TOML configuration.
package config

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/pelletier/go-toml/v2"

	"nought/internal/atomicfile"
	"nought/internal/query"
)

// TLS modes. Plaintext connections are deliberately not representable.
const (
	TLSImplicit = "implicit"
	TLSStartTLS = "starttls"
)

// Deletion settings for an account.
const (
	DeletionDisabled = "disabled"
	DeletionEnabled  = "enabled"
)

// Config is the whole configuration file.
type Config struct {
	Accounts []Account `toml:"account"`
	// TrashDays is how long deleted mail stays in the trash (default 30).
	TrashDays *int `toml:"trash_days,omitempty"`
	// PreferBody picks the version shown when a message has both: "html"
	// (converted to text, the default) or "plain".
	PreferBody string `toml:"prefer_body,omitempty"`
	Rules      []Rule `toml:"rule,omitempty"`
	TUI        TUI    `toml:"tui,omitempty"`
}

// Body versions for PreferBody.
const (
	BodyHTML  = "html"
	BodyPlain = "plain"
)

// Rule acts on newly archived messages that match a query: it adds tags,
// and can make them expire (be deleted for good, locally and from the
// server) a number of days after they arrive.
type Rule struct {
	Query      string   `toml:"query"`
	Tags       []string `toml:"tags,omitempty"`
	ExpireDays int      `toml:"expire_days,omitempty"`
}

// DefaultTrashDays applies when trash_days is not set.
const DefaultTrashDays = 30

// TrashKeepDays returns how long deleted mail stays in the trash.
func (c *Config) TrashKeepDays() int {
	if c.TrashDays == nil {
		return DefaultTrashDays
	}
	return *c.TrashDays
}

// TUI holds terminal UI preferences.
type TUI struct {
	// MarkReadOnOpen marks a message read when it is opened (default true).
	MarkReadOnOpen *bool `toml:"mark_read_on_open,omitempty"`
	// ASCII uses plain characters instead of symbols for status markers.
	ASCII    bool          `toml:"ascii,omitempty"`
	Searches []SavedSearch `toml:"search,omitempty"`
	// Hints shows a line of useful keys for the current screen (default true).
	Hints *bool `toml:"hints,omitempty"`
}

// ShowHints reports whether the key hints line is shown.
func (t TUI) ShowHints() bool { return t.Hints == nil || *t.Hints }

// SavedSearch is a named query shown in the TUI sidebar.
type SavedSearch struct {
	Name  string `toml:"name"`
	Query string `toml:"query"`
}

// ReadOnOpen reports whether opening a message marks it read.
func (t TUI) ReadOnOpen() bool { return t.MarkReadOnOpen == nil || *t.MarkReadOnOpen }

// Account is one mail account.
type Account struct {
	Name            string       `toml:"name"`
	Label           string       `toml:"label,omitempty"`
	Color           string       `toml:"color,omitempty"`
	Email           string       `toml:"email"`
	Username        string       `toml:"username,omitempty"`
	PasswordCommand string       `toml:"password_command,omitempty"`
	Deletion        string       `toml:"deletion"`
	IMAP            Server       `toml:"imap"`
	SMTP            Server       `toml:"smtp,omitempty"`
	Folders         []FolderRule `toml:"folder,omitempty"`
}

// Server is a TLS mail server endpoint.
type Server struct {
	Host string `toml:"host,omitempty"`
	Port int    `toml:"port,omitempty"`
	TLS  string `toml:"tls,omitempty"`
}

// FolderRule maps server folders, by glob, to a policy.
type FolderRule struct {
	Match    string `toml:"match"`
	Action   string `toml:"action,omitempty"`
	KeepDays *int   `toml:"keep_days,omitempty"`
}

// Login returns the IMAP/SMTP username, which defaults to the email address.
func (a *Account) Login() string {
	if a.Username != "" {
		return a.Username
	}
	return a.Email
}

// DeletionEnabled reports whether server deletion was deliberately enabled.
func (a *Account) DeletionEnabled() bool { return a.Deletion == DeletionEnabled }

// Account returns the account with the given name.
func (c *Config) Account(name string) (*Account, bool) {
	for i := range c.Accounts {
		if c.Accounts[i].Name == name {
			return &c.Accounts[i], true
		}
	}
	return nil, false
}

// Load reads and validates the configuration. A missing file is an empty
// configuration, so a fresh install works without setup.
func Load(path string) (*Config, error) {
	data, err := readLimited(path, maxConfigSize)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var c Config
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		if strict, ok := errors.AsType[*toml.StrictMissingError](err); ok {
			return nil, fmt.Errorf("parse config %s: %s", path, strict.String())
		}
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

const maxConfigSize = 1 << 20

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if int64(len(data)) > limit {
		return nil, errors.Join(errors.New("file is larger than 1 MiB"), f.Close())
	}
	return data, f.Close()
}

// Save validates and atomically writes the configuration. Comments in a
// hand-edited file are not preserved.
func Save(path string, c *Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := toml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return atomicfile.WriteBytes(path, 0o600, data)
}

var (
	// Account names become directory names, so they are restricted to a
	// safe, portable alphabet.
	validName  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	validColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	// Hostnames, IPv4 literals, or bracketless IPv6 literals.
	validHost = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$|^[0-9A-Fa-f:.]+$`)
	// Device names Windows reserves in every directory.
	windowsReserved = regexp.MustCompile(`^(?:con|prn|aux|nul|com[0-9]|lpt[0-9])$`)
)

// Validate checks the whole configuration and reports the first problem.
func (c *Config) Validate() error {
	if c.PreferBody != "" && c.PreferBody != BodyHTML && c.PreferBody != BodyPlain {
		return fmt.Errorf("prefer_body must be %q or %q", BodyHTML, BodyPlain)
	}
	if c.TrashDays != nil && (*c.TrashDays < 0 || *c.TrashDays > 3650) {
		return errors.New("trash_days must be between 0 and 3650")
	}
	for i, r := range c.Rules {
		if _, err := query.Parse(r.Query); err != nil || strings.TrimSpace(r.Query) == "" {
			return fmt.Errorf("rule %d: query: %w", i+1, cmp.Or(err, errors.New("must not be empty")))
		}
		if r.ExpireDays < 0 || r.ExpireDays > 3650 {
			return fmt.Errorf("rule %d: expire_days must be between 0 and 3650", i+1)
		}
		if len(r.Tags) == 0 && r.ExpireDays == 0 {
			return fmt.Errorf("rule %d: needs tags or expire_days", i+1)
		}
		for _, t := range r.Tags {
			if t == "" || t != strings.ToLower(t) || strings.ContainsFunc(t, unicode.IsSpace) || hasControl(t) ||
				strings.HasPrefix(t, "+") || strings.HasPrefix(t, "-") {
				return fmt.Errorf("rule %d: tag %q must be lowercase, without spaces or a leading + or -", i+1, t)
			}
			// Deletion goes through expire_days, which writes a valid date.
			if t == "deleted" || strings.HasPrefix(t, "delete-after:") {
				return fmt.Errorf("rule %d: tag %q is reserved; use expire_days", i+1, t)
			}
		}
	}
	for _, s := range c.TUI.Searches {
		if strings.TrimSpace(s.Name) == "" || hasControl(s.Name) || strings.TrimSpace(s.Query) == "" {
			return fmt.Errorf("tui search %q: needs a name and a query", s.Name)
		}
	}
	seen := make(map[string]bool)
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if seen[a.Name] {
			return fmt.Errorf("duplicate account name %q", a.Name)
		}
		seen[a.Name] = true
		if err := a.validate(); err != nil {
			return fmt.Errorf("account %q: %w", a.Name, err)
		}
	}
	return nil
}

// ValidateAccountName checks that name is usable as an account name, which
// is also a directory name in the archive.
func ValidateAccountName(name string) error {
	if !validName.MatchString(name) || windowsReserved.MatchString(name) {
		return errors.New("name must be 1-32 characters of a-z, 0-9, '-' or '_', starting with a letter or digit")
	}
	return nil
}

func (a *Account) validate() error {
	if err := ValidateAccountName(a.Name); err != nil {
		return err
	}
	if _, err := mail.ParseAddress(a.Email); err != nil || strings.ContainsAny(a.Email, "<>") {
		return errors.New("email must be a plain address like me@example.org")
	}
	if hasControl(a.Username) || hasControl(a.PasswordCommand) {
		return errors.New("username and password_command must not contain control characters")
	}
	if a.Color != "" && !validColor.MatchString(a.Color) {
		return errors.New("color must look like #44aa99")
	}
	if hasControl(a.Label) || len([]rune(a.Label)) > 8 {
		return errors.New("label must be at most 8 printable characters")
	}
	switch a.Deletion {
	case "", DeletionDisabled, DeletionEnabled:
	default:
		return fmt.Errorf("deletion must be %q or %q", DeletionDisabled, DeletionEnabled)
	}
	if err := a.IMAP.validate(); err != nil {
		return fmt.Errorf("imap: %w", err)
	}
	if a.SMTP != (Server{}) {
		if err := a.SMTP.validate(); err != nil {
			return fmt.Errorf("smtp: %w", err)
		}
	}
	for _, r := range a.Folders {
		if err := r.validate(); err != nil {
			return fmt.Errorf("folder rule %q: %w", r.Match, err)
		}
	}
	return nil
}

func (s Server) validate() error {
	if len(s.Host) > 253 || !validHost.MatchString(s.Host) {
		return errors.New("host must be a hostname or IP address")
	}
	if s.Port < 1 || s.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if s.TLS != TLSImplicit && s.TLS != TLSStartTLS {
		return fmt.Errorf("tls must be %q or %q", TLSImplicit, TLSStartTLS)
	}
	return nil
}

func hasControl(s string) bool { return strings.IndexFunc(s, unicode.IsControl) >= 0 }
