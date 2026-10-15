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
}

// AccountDiscover finds settings in the built-in provider list. Nothing
// leaves the machine.
var AccountDiscover = Op[DiscoverIn, autoconfig.Result]{
	Name:    "account.discover",
	Summary: "Find server settings for an email address in the built-in provider list (offline)",
	Perm:    Read,
	Run: func(ctx context.Context, env *Env, in DiscoverIn) (autoconfig.Result, error) {
		return discover(ctx, env, in.Email, false)
	},
}

// AccountDiscoverOnline also queries DNS and contacts the provider. It
// requires the highest permission: an agent must not be able to make nought
// contact arbitrary domains (which could leak data through DNS names).
var AccountDiscoverOnline = Op[DiscoverIn, autoconfig.Result]{
	Name:    "account.discover_online",
	Summary: "Find server settings for an email address, querying DNS and the provider",
	Perm:    Delete,
	Run: func(ctx context.Context, env *Env, in DiscoverIn) (autoconfig.Result, error) {
		return discover(ctx, env, in.Email, true)
	},
}

func discover(ctx context.Context, env *Env, email string, online bool) (autoconfig.Result, error) {
	res, err := autoconfig.Discover(ctx, email, online, env.Network)
	switch {
	case errors.Is(err, autoconfig.ErrNotFound):
		return res, fmt.Errorf("no settings found for %s: %w", email, ErrNotFound)
	case errors.Is(err, autoconfig.ErrOAuthOnly):
		return res, err
	case err != nil && res.Source == "":
		return res, usageError("%v", err)
	}
	if res.Contacted == nil {
		res.Contacted = []string{}
	}
	return res, err
}

// AccountCheck connects with settings that are not saved yet, to confirm
// they work before adding the account. Nothing on the server changes. It
// requires the highest permission, like account.add: it sends a password to
// any server and can run a password command.
var AccountCheck = Op[AccountIn, AccountTestOut]{
	Name:    "account.check",
	Summary: "Try account settings before saving them",
	Perm:    Delete,
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
