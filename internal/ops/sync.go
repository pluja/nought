// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/imapsync"
	"github.com/pluja/nought/internal/lock"
	"github.com/pluja/nought/internal/secret"
)

// SyncIn selects accounts to sync. No accounts means all of them.
type SyncIn struct {
	Accounts []string `json:"accounts,omitempty"`
	DryRun   bool     `json:"dry_run,omitempty"`
	// AcceptRemovals applies changes that look like a mass removal (many
	// messages gone from a folder at once), which are otherwise held.
	AcceptRemovals bool `json:"accept_removals,omitempty"`
	// Progress, if set, is called (from sync goroutines, possibly
	// concurrently) as each folder finishes. Frontends use it for live
	// progress.
	Progress func(account string, folder FolderSyncOut) `json:"-"`
}

// SyncOut reports a sync, per account and folder.
type SyncOut struct {
	Accounts []AccountSyncOut `json:"accounts"`
	Outbox   FlushOut         `json:"outbox"` // due mail, sent with the sync
	DryRun   bool             `json:"dry_run"`
}

// AccountSyncOut is one account's result.
type AccountSyncOut struct {
	Account  string          `json:"account"`
	Deletion bool            `json:"deletion"`
	Folders  []FolderSyncOut `json:"folders"`
	// What another client (a phone) did, mirrored here.
	ArchivedElsewhere int `json:"archived_elsewhere"` // left the server's INBOX: out of the inbox here
	DeletedElsewhere  int `json:"deleted_elsewhere"`  // gone from the server: to the trash here
	BackInInbox       int `json:"back_in_inbox"`      // moved back to the server's INBOX
	TrashCleaned      int `json:"trash_cleaned"`      // archived mail deleted from the server's trash
	// RemovalsHeld: many messages disappeared at once, so nothing was
	// applied; sync with AcceptRemovals if that was the user.
	RemovalsHeld bool     `json:"removals_held,omitempty"`
	Notes        []string `json:"notes,omitempty"`
	Error        string   `json:"error,omitempty"`
}

// FolderSyncOut is one server folder's result.
type FolderSyncOut struct {
	Folder      string   `json:"folder"`
	Action      string   `json:"action"`          // "archive" or "ignore"
	KeepDays    int      `json:"keep_days"`       // minimum days on the server once handled
	MaxDays     int      `json:"max_days"`        // deleted after this many days even if not handled; 0: never
	New         int      `json:"new"`             // messages not yet archived
	Fetched     int      `json:"fetched"`         // archived and verified in this run
	Due         int      `json:"due"`             // handled or expired copies due for deletion
	Expunged    int      `json:"expunged"`        // deleted from the server in this run
	Vanished    int      `json:"vanished"`        // moved or deleted by another client
	BackInInbox int      `json:"back_in_inbox"`   // known mail moved back to the INBOX by another client
	Notes       []string `json:"notes,omitempty"` // why something was not done
	Error       string   `json:"error,omitempty"` // the folder stopped here
}

// DescribeOtherDevices says what another client did, as mirrored by sync
// ("2 archived, 1 deleted"), or "" if nothing. It is shared by frontends.
func DescribeOtherDevices(archived, deleted, back int) string {
	var parts []string
	if archived > 0 {
		parts = append(parts, fmt.Sprintf("%d archived", archived))
	}
	if deleted > 0 {
		parts = append(parts, fmt.Sprintf("%d deleted", deleted))
	}
	if back > 0 {
		parts = append(parts, fmt.Sprintf("%d moved back to the inbox", back))
	}
	return strings.Join(parts, ", ")
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

// acquireLock makes sync, reindex and permanent deletion mutually
// exclusive: each of them assumes the server copies it sees are current.
func acquireLock(env *Env) (release func(), err error) {
	l, err := lock.Acquire(env.Paths.LockFile)
	if errors.Is(err, lock.ErrLocked) {
		return nil, fmt.Errorf("a sync, reindex or deletion is running: %w", ErrBusy)
	}
	if err != nil {
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}

func runSync(ctx context.Context, env *Env, in SyncIn) (SyncOut, error) {
	out := SyncOut{DryRun: in.DryRun, Accounts: []AccountSyncOut{}}
	accounts, err := selectAccounts(env, in.Accounts)
	if err != nil {
		return out, err
	}
	release, err := acquireLock(env)
	if err != nil {
		return out, err
	}
	defer release()
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
		wg.Go(func() { out.Accounts[i] = syncAccount(ctx, env, a, passwords[i], in) })
	}
	wg.Wait()

	// A sync is when nought is online: due mail goes out with it. A send
	// failure stays in the outbox and is reported, not fatal to the sync.
	if !in.DryRun {
		flushed, err := Call(ctx, env, Send, OutboxFlush, FlushIn{})
		if err != nil {
			flushed.Notes = append(flushed.Notes, err.Error())
		}
		out.Outbox = flushed
	}

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
	// Deleted and expired mail whose server copies are gone now leaves the
	// archive for good.
	if !in.DryRun {
		if _, err := purge(ctx, env); err != nil {
			return out, err
		}
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if failed > 0 {
		return out, fmt.Errorf("%d accounts or folders did not finish: %w", failed, ErrPartial)
	}
	if out.Outbox.Failed > 0 {
		return out, fmt.Errorf("%d messages were not sent (see nou outbox list): %w", out.Outbox.Failed, ErrPartial)
	}
	return out, nil
}

func firstOr(s []uint32, def uint32) uint32 {
	if len(s) == 0 {
		return def
	}
	return s[0]
}

func lastOr(s []uint32, def uint32) uint32 {
	if len(s) == 0 {
		return def
	}
	return s[len(s)-1]
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

func syncAccount(ctx context.Context, env *Env, a config.Account, password string, in SyncIn) AccountSyncOut {
	out := AccountSyncOut{Account: a.Name, Deletion: a.DeletionEnabled(), Folders: []FolderSyncOut{}}
	conn, err := imapsync.Dial(ctx, a.IMAP, env.TLS, a.Login(), password, env.IMAPTimeout)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	defer func() { _ = conn.Close() }()
	ix, err := env.Index(ctx)
	if err == nil {
		err = syncPhases(ctx, env, ix, conn, &a, in, &out)
	}
	if err != nil {
		out.Error = err.Error()
	}
	return out
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
		out.Folders = append(out.Folders, FolderPolicyOut{Folder: f.Name, Policy: folderPolicy(a, f)})
	}
	return out, nil
}
