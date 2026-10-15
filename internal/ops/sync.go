// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"nought/internal/config"
	"nought/internal/imapsync"
	"nought/internal/index"
	"nought/internal/lock"
	"nought/internal/secret"
)

// SyncIn selects accounts to sync. No accounts means all of them.
type SyncIn struct {
	Accounts []string `json:"accounts,omitempty"`
	DryRun   bool     `json:"dry_run,omitempty"`
}

// SyncOut reports a sync, per account and folder.
type SyncOut struct {
	Accounts []AccountSyncOut `json:"accounts"`
	DryRun   bool             `json:"dry_run"`
}

// AccountSyncOut is one account's result.
type AccountSyncOut struct {
	Account  string          `json:"account"`
	Deletion bool            `json:"deletion"`
	Folders  []FolderSyncOut `json:"folders"`
	Error    string          `json:"error,omitempty"`
}

// FolderSyncOut is one server folder's result.
type FolderSyncOut struct {
	Folder   string   `json:"folder"`
	Action   string   `json:"action"`          // "archive" or "ignore"
	KeepDays int      `json:"keep_days"`       // days mail stays on the server
	New      int      `json:"new"`             // messages not yet archived
	Fetched  int      `json:"fetched"`         // archived and verified in this run
	Due      int      `json:"due"`             // verified copies old enough to delete
	Expunged int      `json:"expunged"`        // deleted from the server in this run
	Notes    []string `json:"notes,omitempty"` // why something was not done
	Error    string   `json:"error,omitempty"` // the folder stopped here
}

// maxMessageSize bounds a fetched message; larger ones stay on the server.
const maxMessageSize = 256 << 20

// SyncRun fetches new mail into the archive and, for accounts with deletion
// enabled, deletes from the server the messages whose local copies are
// verified and whose folder policy says they have been kept long enough.
var SyncRun = Op[SyncIn, SyncOut]{
	Name:    "sync.run",
	Summary: "Archive new mail and delete verified mail from servers per folder policy",
	Perm:    Delete,
	Run:     runSync,
}

func runSync(ctx context.Context, env *Env, in SyncIn) (SyncOut, error) {
	out := SyncOut{DryRun: in.DryRun, Accounts: []AccountSyncOut{}}
	accounts, err := selectAccounts(env, in.Accounts)
	if err != nil {
		return out, err
	}
	l, err := lock.Acquire(env.Paths.LockFile)
	if errors.Is(err, lock.ErrLocked) {
		return out, fmt.Errorf("another sync or reindex is running: %w", ErrBusy)
	}
	if err != nil {
		return out, err
	}
	defer func() { _ = l.Release() }()
	if _, err := env.Index(ctx); err != nil {
		return out, err
	}

	// Passwords are looked up one account at a time, so password commands
	// that prompt (gpg-agent, pinentry) never compete. Accounts then sync in
	// parallel, each with its own connection and Maildir.
	out.Accounts = make([]AccountSyncOut, len(accounts))
	passwords := make([]string, len(accounts))
	for i, a := range accounts {
		pw, err := secret.Password(ctx, env.Secrets, a.Name, a.PasswordCommand)
		if err != nil {
			out.Accounts[i] = AccountSyncOut{Account: a.Name, Deletion: a.DeletionEnabled(), Folders: []FolderSyncOut{}, Error: err.Error()}
			continue
		}
		passwords[i] = pw
	}
	var wg sync.WaitGroup
	for i, a := range accounts {
		if out.Accounts[i].Error != "" {
			continue
		}
		wg.Go(func() { out.Accounts[i] = syncAccount(ctx, env, a, passwords[i], in.DryRun) })
	}
	wg.Wait()

	fetched, failed := 0, 0
	for _, a := range out.Accounts {
		if a.Error != "" {
			failed++
		}
		for _, f := range a.Folders {
			fetched += f.Fetched
			if f.Error != "" {
				failed++
			}
		}
	}
	if fetched > 0 {
		if err := writeTagsDump(ctx, env); err != nil {
			return out, err
		}
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if failed > 0 {
		return out, fmt.Errorf("%d accounts or folders did not finish: %w", failed, ErrPartial)
	}
	return out, nil
}

func selectAccounts(env *Env, names []string) ([]config.Account, error) {
	cfg, err := env.Config()
	if err != nil {
		return nil, err
	}
	if len(cfg.Accounts) == 0 {
		return nil, usageError("no accounts configured (see 'nou account add')")
	}
	if len(names) == 0 {
		return slices.Clone(cfg.Accounts), nil
	}
	var out []config.Account
	for _, n := range slices.Compact(slices.Sorted(slices.Values(names))) {
		a, ok := cfg.Account(n)
		if !ok {
			return nil, fmt.Errorf("account %q: %w", n, ErrNotFound)
		}
		out = append(out, *a)
	}
	return out, nil
}

func syncAccount(ctx context.Context, env *Env, a config.Account, password string, dryRun bool) AccountSyncOut {
	out := AccountSyncOut{Account: a.Name, Deletion: a.DeletionEnabled(), Folders: []FolderSyncOut{}}
	conn, err := imapsync.Dial(ctx, a.IMAP, env.TLS, a.Login(), password, env.IMAPTimeout)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer func() { _ = conn.Close() }()

	folders, err := conn.Folders()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	for _, f := range folders {
		policy := a.Policy(f.Name)
		fo := FolderSyncOut{Folder: f.Name, Action: policy.Action, KeepDays: policy.KeepDays}
		if policy.Action != config.ActionIgnore {
			if err := syncFolder(ctx, env, conn, &a, f, policy, dryRun, &fo); err != nil {
				fo.Error = err.Error()
			}
		}
		out.Folders = append(out.Folders, fo)
		if ctx.Err() != nil {
			break
		}
	}
	return out
}

// syncFolder archives a folder's new messages, then deletes its due ones.
// State and server copies are keyed by the server's raw folder name, so two
// folders whose display names collide never share state.
func syncFolder(ctx context.Context, env *Env, conn *imapsync.Conn, a *config.Account, f imapsync.Folder,
	policy config.Policy, dryRun bool, out *FolderSyncOut) error {
	ix, err := env.Index(ctx)
	if err != nil {
		return err
	}
	mb, err := conn.Select(f.Raw, dryRun)
	if err != nil {
		return err
	}
	st, found, err := ix.SyncState(ctx, a.Name, f.Raw)
	if err != nil {
		return err
	}
	if !found || st.UIDValidity != mb.UIDValidity {
		if found {
			out.Notes = append(out.Notes, "UIDVALIDITY changed: the folder is fetched again and matched to the archive")
		}
		if !dryRun {
			if err := ix.ResetFolder(ctx, a.Name, f.Raw, mb.UIDValidity); err != nil {
				return err
			}
		}
		st = index.SyncState{UIDValidity: mb.UIDValidity}
	}

	uids, err := conn.UIDsAfter(st.LastUID)
	if err != nil {
		return err
	}
	out.New = len(uids)
	if !dryRun {
		err := fetchNew(ctx, env, ix, conn, a.Name, f, uids, &st, out)
		if errors.Is(err, index.ErrCopyConflict) {
			// The server reused a UID for other content. Trust no UID in
			// this folder: forget its pending copies, delete nothing now,
			// and fetch it again from the start next time.
			out.Notes = append(out.Notes, "the server reused a message UID; nothing was deleted, and the folder will be fetched again")
			return ix.ForgetFolder(ctx, a.Name, f.Raw, st.UIDValidity)
		}
		if err != nil {
			return err
		}
	}
	return expungeDue(ctx, env, ix, conn, a, f.Raw, st.UIDValidity, policy, dryRun, out)
}

// AccountTestOut reports what nou sees on an account's server.
type AccountTestOut struct {
	Account      string            `json:"account"`
	Capabilities []string          `json:"capabilities"`
	UIDPlus      bool              `json:"uidplus"`
	Folders      []FolderPolicyOut `json:"folders"`
}

// FolderPolicyOut is a server folder and what sync would do with it.
type FolderPolicyOut struct {
	Folder string        `json:"folder"`
	Policy config.Policy `json:"policy"`
}

// AccountTest connects, logs in, and shows each folder's policy, without
// changing anything.
var AccountTest = Op[AccountName, AccountTestOut]{
	Name:    "account.test",
	Summary: "Connect to an account's server and show what sync would do with each folder",
	Perm:    Read,
	Run: func(ctx context.Context, env *Env, in AccountName) (AccountTestOut, error) {
		a, err := account(env, in.Name)
		if err != nil {
			return AccountTestOut{}, err
		}
		pw, err := secret.Password(ctx, env.Secrets, a.Name, a.PasswordCommand)
		if err != nil {
			return AccountTestOut{}, err
		}
		return testServer(ctx, env, a, pw)
	},
}

// testServer logs in to an account's IMAP server and lists its folders
// with their policies. Nothing on the server changes.
func testServer(ctx context.Context, env *Env, a *config.Account, password string) (AccountTestOut, error) {
	conn, err := imapsync.Dial(ctx, a.IMAP, env.TLS, a.Login(), password, env.IMAPTimeout)
	if err != nil {
		return AccountTestOut{}, err
	}
	defer func() { _ = conn.Close() }()
	folders, err := conn.Folders()
	if err != nil {
		return AccountTestOut{}, err
	}
	out := AccountTestOut{Account: a.Name, Capabilities: conn.Capabilities(), UIDPlus: conn.HasUIDPlus(),
		Folders: []FolderPolicyOut{}}
	for _, f := range folders {
		out.Folders = append(out.Folders, FolderPolicyOut{Folder: f.Name, Policy: a.Policy(f.Name)})
	}
	return out, nil
}
