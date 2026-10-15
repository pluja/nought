// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"

	"nought/internal/autoconfig"
	"nought/internal/config"
	"nought/internal/secret"
)

// DiscoverIn asks for an email address's server settings.
type DiscoverIn struct {
	Email string `json:"email"`
	// Online allows DNS lookups and contacting the provider. Without it,
	// only the built-in provider list is used and nothing leaves the machine.
	Online bool `json:"online,omitempty"`
}

// AccountDiscover finds IMAP and SMTP settings for an email address.
var AccountDiscover = Op[DiscoverIn, autoconfig.Result]{
	Name:    "account.discover",
	Summary: "Find server settings for an email address (offline unless online is allowed)",
	Perm:    Read,
	Run: func(ctx context.Context, env *Env, in DiscoverIn) (autoconfig.Result, error) {
		res, err := autoconfig.Discover(ctx, in.Email, in.Online, env.Network)
		switch {
		case errors.Is(err, autoconfig.ErrNotFound):
			return res, fmt.Errorf("no settings found for %s: %w", in.Email, ErrNotFound)
		case errors.Is(err, autoconfig.ErrOAuthOnly):
			return res, fmt.Errorf("%w: %w", err, ErrUsage)
		case err != nil && res.Source == "":
			return res, usageError("%v", err)
		}
		if res.Contacted == nil {
			res.Contacted = []string{}
		}
		return res, err
	},
}

// AccountCheck connects with settings that are not saved yet, to confirm
// they work before adding the account. Nothing on the server changes.
var AccountCheck = Op[AccountIn, AccountTestOut]{
	Name:    "account.check",
	Summary: "Try account settings before saving them",
	Perm:    Read,
	Run: func(ctx context.Context, env *Env, in AccountIn) (AccountTestOut, error) {
		if in.Name == "" {
			in.Name = "check"
		}
		a := config.Account{
			Name: in.Name, Email: in.Email, Username: in.Username, PasswordCommand: in.PasswordCommand,
			IMAP: server(in.IMAPHost, in.IMAPPort, in.IMAPTLS, 993, 143), Folders: DefaultFolderRules(),
		}
		if err := (&config.Config{Accounts: []config.Account{a}}).Validate(); err != nil {
			return AccountTestOut{}, usageError("%v", err)
		}
		pw := in.Password
		if in.PasswordCommand != "" {
			var err error
			if pw, err = secret.Password(ctx, secret.Memory{}, a.Name, in.PasswordCommand); err != nil {
				return AccountTestOut{}, err
			}
		}
		return testServer(ctx, env, &a, pw)
	},
}
