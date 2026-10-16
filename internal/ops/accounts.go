// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/secret"
)

// AccountIn describes a new account.
type AccountIn struct {
	Name            string `json:"name"`
	Email           string `json:"email"`
	Username        string `json:"username,omitempty"` // defaults to Email
	Label           string `json:"label,omitempty"`
	Color           string `json:"color,omitempty"`
	IMAPHost        string `json:"imap_host"`
	IMAPPort        int    `json:"imap_port,omitempty"` // default 993 (implicit TLS) or 143 (STARTTLS)
	IMAPTLS         string `json:"imap_tls,omitempty"`  // "implicit" (default) or "starttls"
	SMTPHost        string `json:"smtp_host,omitempty"`
	SMTPPort        int    `json:"smtp_port,omitempty"` // default 465 (implicit TLS) or 587 (STARTTLS)
	SMTPTLS         string `json:"smtp_tls,omitempty"`
	PasswordCommand string `json:"password_command,omitempty"`
	Password        string `json:"password,omitempty"` // stored in the OS keyring, never in the config
}

// AccountOut is an account as reported to users; it never contains secrets.
type AccountOut struct {
	Name           string              `json:"name"`
	Email          string              `json:"email"`
	Username       string              `json:"username"`
	Label          string              `json:"label,omitempty"`
	Color          string              `json:"color,omitempty"`
	IMAP           config.Server       `json:"imap"`
	SMTP           *config.Server      `json:"smtp,omitempty"`
	Deletion       string              `json:"deletion"`
	PasswordSource string              `json:"password_source"` // "keyring" or "command"
	Folders        []config.FolderRule `json:"folders"`
	Archive        string              `json:"archive"`
}

// DefaultFolderRules are written for new accounts. Drafts and templates are
// work in progress, and junk and trash are unwanted; none of them is
// archived or deleted from the server, wherever the server puts them
// ("Drafts", "INBOX.Drafts", "[Gmail]/Drafts", ...). Matching ignores case.
func DefaultFolderRules() []config.FolderRule {
	return []config.FolderRule{
		{Match: "*{Draft,Template,Junk,Spam,Trash,Deleted}*", Action: config.ActionIgnore},
	}
}

// AccountAdd adds an account. Server deletion starts disabled: the user
// enables it deliberately once the archive looks right.
var AccountAdd = Op[AccountIn, AccountOut]{
	Name:    "account.add",
	Summary: "Add a mail account",
	Perm:    Delete,
	Run:     runAccountAdd,
}

func runAccountAdd(ctx context.Context, env *Env, in AccountIn) (AccountOut, error) {
	cfg, err := env.Config()
	if err != nil {
		return AccountOut{}, err
	}
	if _, exists := cfg.Account(in.Name); exists {
		return AccountOut{}, usageError("account %q already exists", in.Name)
	}
	if in.Password == "" && in.PasswordCommand == "" {
		return AccountOut{}, usageError("a password (--password-stdin) or --password-command is required")
	}
	if in.Password != "" && in.PasswordCommand != "" {
		return AccountOut{}, usageError("give either a password or a password command, not both")
	}
	a := config.Account{
		Name: in.Name, Email: in.Email, Username: in.Username, Label: in.Label, Color: in.Color,
		PasswordCommand: in.PasswordCommand, Deletion: config.DeletionDisabled,
		IMAP:    server(in.IMAPHost, in.IMAPPort, in.IMAPTLS, 993, 143),
		Folders: DefaultFolderRules(),
	}
	if in.SMTPHost != "" {
		a.SMTP = server(in.SMTPHost, in.SMTPPort, in.SMTPTLS, 465, 587)
	}

	next := withAccounts(cfg, append(slices.Clone(cfg.Accounts), a))
	if err := next.Validate(); err != nil {
		return AccountOut{}, usageError("%v", err)
	}
	if in.Password != "" {
		if err := env.Secrets.Set(a.Name, in.Password); err != nil {
			return AccountOut{}, err
		}
	}
	if err := saveConfig(env, next); err != nil {
		if in.Password != "" {
			err = errors.Join(err, env.Secrets.Delete(a.Name))
		}
		return AccountOut{}, err
	}
	return accountOut(env, &next.Accounts[len(next.Accounts)-1]), nil
}

func server(host string, port int, tls string, implicitPort, starttlsPort int) config.Server {
	if tls == "" {
		tls = config.TLSImplicit
	}
	if port == 0 {
		port = implicitPort
		if tls == config.TLSStartTLS {
			port = starttlsPort
		}
	}
	return config.Server{Host: host, Port: port, TLS: tls}
}

// withAccounts is cfg with other accounts, every other setting kept.
func withAccounts(cfg *config.Config, accounts []config.Account) *config.Config {
	next := *cfg
	next.Accounts = accounts
	return &next
}

func saveConfig(env *Env, next *config.Config) error {
	if err := config.Save(env.Paths.ConfigFile, next); err != nil {
		return err
	}
	env.mu.Lock()
	env.config = next
	env.mu.Unlock()
	return nil
}

func accountOut(env *Env, a *config.Account) AccountOut {
	out := AccountOut{
		Name: a.Name, Email: a.Email, Username: a.Login(), Label: a.Label, Color: a.Color,
		IMAP: a.IMAP, Deletion: a.Deletion, PasswordSource: "keyring",
		Folders: a.Folders, Archive: env.Paths.AccountArchive(a.Name),
	}
	if out.Deletion == "" {
		out.Deletion = config.DeletionDisabled
	}
	if out.Folders == nil {
		out.Folders = []config.FolderRule{}
	}
	if a.SMTP != (config.Server{}) {
		smtp := a.SMTP
		out.SMTP = &smtp
	}
	if a.PasswordCommand != "" {
		out.PasswordSource = "command"
	}
	return out
}

// AccountList lists configured accounts.
var AccountList = Op[struct{}, []AccountOut]{
	Name:    "account.list",
	Summary: "List mail accounts",
	Perm:    Read,
	Run: func(_ context.Context, env *Env, _ struct{}) ([]AccountOut, error) {
		cfg, err := env.Config()
		if err != nil {
			return nil, err
		}
		out := []AccountOut{}
		for i := range cfg.Accounts {
			out = append(out, accountOut(env, &cfg.Accounts[i]))
		}
		return out, nil
	},
}

// AccountName names one account.
type AccountName struct {
	Name string `json:"name"`
}

// AccountShow shows one account.
var AccountShow = Op[AccountName, AccountOut]{
	Name:    "account.show",
	Summary: "Show a mail account",
	Perm:    Read,
	Run: func(_ context.Context, env *Env, in AccountName) (AccountOut, error) {
		a, err := account(env, in.Name)
		if err != nil {
			return AccountOut{}, err
		}
		return accountOut(env, a), nil
	},
}

func account(env *Env, name string) (*config.Account, error) {
	cfg, err := env.Config()
	if err != nil {
		return nil, err
	}
	a, ok := cfg.Account(name)
	if !ok {
		return nil, fmt.Errorf("account %q: %w", name, ErrNotFound)
	}
	return a, nil
}

// AccountSetIn changes account settings. Empty fields are left unchanged.
type AccountSetIn struct {
	Name     string `json:"name"`
	Deletion string `json:"deletion,omitempty"` // "enabled" or "disabled"
	Label    string `json:"label,omitempty"`
	Color    string `json:"color,omitempty"`
	SMTPHost string `json:"smtp_host,omitempty"` // with SMTPPort and SMTPTLS: the server for sending
	SMTPPort int    `json:"smtp_port,omitempty"`
	SMTPTLS  string `json:"smtp_tls,omitempty"`
}

// AccountSet changes account settings, including turning server deletion
// on or off.
var AccountSet = Op[AccountSetIn, AccountOut]{
	Name:    "account.set",
	Summary: "Change account settings (e.g. enable server deletion)",
	Perm:    Delete,
	Run: func(_ context.Context, env *Env, in AccountSetIn) (AccountOut, error) {
		cfg, err := env.Config()
		if err != nil {
			return AccountOut{}, err
		}
		if _, err := account(env, in.Name); err != nil {
			return AccountOut{}, err
		}
		if in.Deletion == "" && in.Label == "" && in.Color == "" && in.SMTPHost == "" {
			return AccountOut{}, usageError("nothing to change")
		}
		next := withAccounts(cfg, slices.Clone(cfg.Accounts))
		a, _ := next.Account(in.Name)
		if in.Deletion != "" {
			a.Deletion = in.Deletion
		}
		if in.Label != "" {
			a.Label = in.Label
		}
		if in.Color != "" {
			a.Color = in.Color
		}
		if in.SMTPHost != "" {
			a.SMTP = server(in.SMTPHost, in.SMTPPort, in.SMTPTLS, 465, 587)
		}
		if err := next.Validate(); err != nil {
			return AccountOut{}, usageError("%v", err)
		}
		if err := saveConfig(env, next); err != nil {
			return AccountOut{}, err
		}
		return accountOut(env, a), nil
	},
}

// AccountRemoveOut reports a removal.
type AccountRemoveOut struct {
	Name    string `json:"name"`
	Archive string `json:"archive"` // kept; delete it yourself if you really want to
}

// AccountRemove removes an account from the configuration and its password
// from the keyring. The local archive is kept.
var AccountRemove = Op[AccountName, AccountRemoveOut]{
	Name:    "account.remove",
	Summary: "Remove a mail account (the local archive is kept)",
	Perm:    Delete,
	Run: func(_ context.Context, env *Env, in AccountName) (AccountRemoveOut, error) {
		cfg, err := env.Config()
		if err != nil {
			return AccountRemoveOut{}, err
		}
		a, err := account(env, in.Name)
		if err != nil {
			return AccountRemoveOut{}, err
		}
		usesKeyring := a.PasswordCommand == ""
		next := withAccounts(cfg, slices.DeleteFunc(slices.Clone(cfg.Accounts), func(acct config.Account) bool {
			return acct.Name == in.Name
		}))
		if err := saveConfig(env, next); err != nil {
			return AccountRemoveOut{}, err
		}
		if usesKeyring {
			if err := env.Secrets.Delete(in.Name); err != nil && !errors.Is(err, secret.ErrNotFound) {
				env.Log.Warn("could not remove the password from the keyring", "account", in.Name, "err", err)
			}
		}
		return AccountRemoveOut{Name: in.Name, Archive: env.Paths.AccountArchive(in.Name)}, nil
	},
}

// AccountStatusOut summarizes one account for status displays.
type AccountStatusOut struct {
	Account    string     `json:"account"`
	Label      string     `json:"label,omitempty"`
	Deletion   bool       `json:"deletion"`
	OnServer   int        `json:"on_server"` // archived messages still on the server
	LastSynced *time.Time `json:"last_synced,omitempty"`
}

// AccountStatus reports each account's deletion setting, last sync, and
// how many archived messages are still on its server.
var AccountStatus = Op[struct{}, []AccountStatusOut]{
	Name:    "account.status",
	Summary: "Show each account's deletion setting, last sync, and mail still on the server",
	Perm:    Read,
	Run: func(ctx context.Context, env *Env, _ struct{}) ([]AccountStatusOut, error) {
		cfg, err := env.Config()
		if err != nil {
			return nil, err
		}
		ix, err := env.Index(ctx)
		if err != nil {
			return nil, err
		}
		out := []AccountStatusOut{}
		for _, a := range cfg.Accounts {
			st, err := ix.ServerStatus(ctx, a.Name)
			if err != nil {
				return nil, err
			}
			s := AccountStatusOut{Account: a.Name, Label: a.Label, Deletion: a.DeletionEnabled(), OnServer: st.OnServer}
			if !st.LastSynced.IsZero() {
				t := st.LastSynced
				s.LastSynced = &t
			}
			out = append(out, s)
		}
		return out, nil
	},
}
