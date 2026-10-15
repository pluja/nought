// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	Due         int      `json:"due"`             // verified copies old enough to delete
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
	for _, p := range []struct {
		n    int
		what string
	}{{archived, "archived"}, {deleted, "deleted"}, {back, "moved back to the inbox"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
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

// syncPhases runs an account's sync: (1) per folder, archive new mail and
// check which known copies are still there; (2) apply what other clients
// did, only if every folder synced cleanly; (3) per folder, delete what is
// due; then empty known mail from the server's trash.
func syncPhases(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	in SyncIn, out *AccountSyncOut) error {
	folders, err := conn.Folders()
	if err != nil {
		return err
	}
	clean := true
	var vanished []index.ServerCopy
	listed := map[string]bool{}
	for _, f := range folders {
		listed[f.Raw] = true
		policy := a.Policy(f.Name)
		fo := FolderSyncOut{Folder: f.Name, Action: policy.Action, KeepDays: policy.KeepDays, MaxDays: policy.MaxDays}
		if policy.Action != config.ActionIgnore {
			gone, ok, held := scanFolder(ctx, env, ix, conn, a, f, in, &fo)
			clean = clean && ok
			out.RemovalsHeld = out.RemovalsHeld || held
			vanished = append(vanished, gone...)
			out.BackInInbox += fo.BackInInbox
		}
		out.Folders = append(out.Folders, fo)
		if in.Progress != nil {
			in.Progress(a.Name, fo)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	gone, held, err := vanishedFolders(ctx, ix, a.Name, listed, in.AcceptRemovals, out)
	if err != nil {
		return err
	}
	vanished = append(vanished, gone...)
	out.RemovalsHeld = out.RemovalsHeld || held
	switch {
	case clean && !out.RemovalsHeld:
		if err := applyVanished(ctx, env, ix, a.Name, vanished, in.DryRun, out); err != nil {
			return err
		}
	case len(vanished) > 0:
		out.Notes = append(out.Notes, "some folders did not sync completely; "+
			"changes made on other devices are applied next time")
	}
	for i, f := range folders {
		fo := &out.Folders[i]
		if fo.Action == config.ActionIgnore || fo.Error != "" {
			continue
		}
		if err := expungeFolder(ctx, env, ix, conn, a, f, in.DryRun, fo); err != nil {
			fo.Error = err.Error()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	// The archive is safe whatever happens here: a failure is only a note.
	if a.DeletionEnabled() && !in.DryRun {
		if err := cleanServerTrash(ctx, env, ix, conn, a, folders, out); err != nil {
			out.Notes = append(out.Notes, "the server's trash was not emptied: "+err.Error())
		}
	}
	return nil
}

// scanFolder is phase 1 for one folder: archive its new mail, then find
// the known copies that are gone. ok is false if the folder's state cannot
// be trusted in this run; held reports a mass removal that was held.
func scanFolder(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	f imapsync.Folder, in SyncIn, out *FolderSyncOut) (gone []index.ServerCopy, ok, held bool) {
	uidValidity, ok, err := fetchFolder(ctx, env, ix, conn, a, f, in, out)
	if err == nil {
		gone, held, err = checkPresence(ctx, ix, conn, a.Name, f, uidValidity, in.AcceptRemovals, out)
	}
	if err != nil {
		out.Error = err.Error()
		return nil, false, false
	}
	return gone, ok && !held, held
}

// fetchFolder selects a folder and archives its new messages. ok is false
// when the folder's state could not be trusted in this run (its
// UIDVALIDITY changed, or a UID was reused). State and server copies are
// keyed by the server's raw folder name, so folders whose display names
// collide never share state.
func fetchFolder(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	f imapsync.Folder, in SyncIn, out *FolderSyncOut) (uidValidity uint32, ok bool, err error) {
	mb, err := conn.Select(f.Raw, in.DryRun)
	if err != nil {
		return 0, false, err
	}
	st, found, err := ix.SyncState(ctx, a.Name, f.Raw)
	if err != nil {
		return 0, false, err
	}
	ok = true
	if !found || st.UIDValidity != mb.UIDValidity {
		if found {
			out.Notes = append(out.Notes, "UIDVALIDITY changed: the folder is fetched again and matched to the archive")
			ok = false
		}
		if !in.DryRun {
			if err := ix.ResetFolder(ctx, a.Name, f.Raw, mb.UIDValidity); err != nil {
				return 0, false, err
			}
		}
		st = index.SyncState{UIDValidity: mb.UIDValidity}
	}
	// New UIDs after an earlier sync are moves and deliveries; a fetch from
	// the start (new folder, reset, reused UID) only finds known mail again.
	incremental := st.LastUID > 0

	uids, err := conn.UIDsAfter(st.LastUID)
	if err != nil {
		return 0, false, err
	}
	out.New = len(uids)
	// Numbers only: enough to diagnose servers with unstable UIDs.
	env.Log.Debug("folder state", "account", a.Name, "folder", f.Name,
		"stored_found", found, "stored_uidvalidity", st.UIDValidity, "stored_last_uid", st.LastUID,
		"server_uidvalidity", mb.UIDValidity, "server_messages", mb.Messages,
		"new_uids", len(uids), "first_new_uid", firstOr(uids, 0), "last_new_uid", lastOr(uids, 0))
	if !in.DryRun {
		err := fetchNew(ctx, env, ix, conn, a.Name, f, uids, &st, incremental, out)
		if errors.Is(err, index.ErrCopyConflict) {
			// The server reused a UID for other content. Trust no UID in
			// this folder: forget its pending copies, delete nothing now,
			// and fetch it again from the start next time.
			out.Notes = append(out.Notes, "the server reused a message UID; nothing was deleted, and the folder will be fetched again")
			return mb.UIDValidity, false, ix.ForgetFolder(ctx, a.Name, f.Raw, st.UIDValidity)
		}
		if err != nil {
			return 0, false, err
		}
	}
	return mb.UIDValidity, ok, nil
}

// expungeFolder deletes the folder's due messages and records the sync.
func expungeFolder(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	f imapsync.Folder, dryRun bool, out *FolderSyncOut) error {
	mb, err := conn.Select(f.Raw, dryRun)
	if err != nil {
		return err
	}
	if err := expungeDue(ctx, env, ix, conn, a, f.Raw, mb.UIDValidity, a.Policy(f.Name), dryRun, out); err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	return ix.MarkSynced(ctx, a.Name, f.Raw, env.Now())
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
