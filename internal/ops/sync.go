// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"nought/internal/config"
	"nought/internal/imapsync"
	"nought/internal/index"
	"nought/internal/journal"
	"nought/internal/lock"
	"nought/internal/secret"
	"nought/internal/store"
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

	// Accounts sync in parallel; each uses its own connection and Maildir.
	out.Accounts = make([]AccountSyncOut, len(accounts))
	var wg sync.WaitGroup
	for i, a := range accounts {
		wg.Go(func() { out.Accounts[i] = syncAccount(ctx, env, a, in.DryRun) })
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
	for _, n := range names {
		a, ok := cfg.Account(n)
		if !ok {
			return nil, fmt.Errorf("account %q: %w", n, ErrNotFound)
		}
		out = append(out, *a)
	}
	return out, nil
}

func syncAccount(ctx context.Context, env *Env, a config.Account, dryRun bool) AccountSyncOut {
	out := AccountSyncOut{Account: a.Name, Deletion: a.DeletionEnabled(), Folders: []FolderSyncOut{}}
	conn, err := connect(ctx, env, &a)
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

func connect(ctx context.Context, env *Env, a *config.Account) (*imapsync.Conn, error) {
	pw, err := secret.Password(ctx, env.Secrets, a.Name, a.PasswordCommand)
	if err != nil {
		return nil, err
	}
	return imapsync.Dial(ctx, a.IMAP, env.TLS, a.Login(), pw)
}

// syncFolder archives a folder's new messages, then deletes its due ones.
func syncFolder(ctx context.Context, env *Env, conn *imapsync.Conn, a *config.Account, f imapsync.Folder,
	policy config.Policy, dryRun bool, out *FolderSyncOut) error {
	ix, err := env.Index(ctx)
	if err != nil {
		return err
	}
	mb, err := conn.Select(f.Raw)
	if err != nil {
		return err
	}
	st, found, err := ix.SyncState(ctx, a.Name, f.Name)
	if err != nil {
		return err
	}
	if !found || st.UIDValidity != mb.UIDValidity {
		if found {
			out.Notes = append(out.Notes, "UIDVALIDITY changed: the folder is fetched again and matched to the archive")
		}
		if !dryRun {
			if err := ix.ResetFolder(ctx, a.Name, f.Name, mb.UIDValidity); err != nil {
				return err
			}
		}
		st = index.SyncState{UIDValidity: mb.UIDValidity}
	}

	infos, err := conn.List(st.LastUID)
	if err != nil {
		return err
	}
	out.New = len(infos)
	if !dryRun {
		if err := fetchNew(ctx, env, ix, conn, a.Name, f.Name, infos, &st, out); err != nil {
			return err
		}
	}
	return expungeDue(ctx, env, ix, conn, a, f.Name, st.UIDValidity, policy, dryRun, out)
}

func fetchNew(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, account, folder string,
	infos []imapsync.MessageInfo, st *index.SyncState, out *FolderSyncOut) error {
	tags := []string{folderTag(folder)}
	if strings.EqualFold(folder, "INBOX") {
		tags = append(tags, TagInbox)
	}
	for _, info := range infos {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch raw, err := fetchOne(conn, info); {
		case errors.Is(err, errSkip):
			out.Notes = append(out.Notes, fmt.Sprintf("uid %d: %v; left on the server", info.UID, err))
		case err != nil:
			return err
		default:
			// Tags only apply to new messages: a message already archived
			// (and maybe already triaged) keeps its tags.
			if _, _, err := archive(ctx, env, account, raw, flagsOf(info), tags); err != nil {
				return fmt.Errorf("archive uid %d: %w", info.UID, err)
			}
			if err := ix.RecordCopy(ctx, index.ServerCopy{
				Account: account, Folder: folder, UIDValidity: st.UIDValidity, UID: info.UID,
				Hash: store.Hash(raw), InternalDate: info.InternalDate,
			}, env.Now()); err != nil {
				return err
			}
			out.Fetched++
		}
		st.LastUID = info.UID
		if err := ix.SetSyncState(ctx, account, folder, *st); err != nil {
			return err
		}
	}
	return nil
}

var errSkip = errors.New("skipped")

func fetchOne(conn *imapsync.Conn, info imapsync.MessageInfo) ([]byte, error) {
	if info.Size > maxMessageSize {
		return nil, fmt.Errorf("%w: larger than %d MiB", errSkip, maxMessageSize>>20)
	}
	raw, err := conn.Fetch(info.UID)
	if errors.Is(err, imapsync.ErrGone) {
		return nil, fmt.Errorf("%w: deleted on the server meanwhile", errSkip)
	}
	return raw, err
}

func flagsOf(info imapsync.MessageInfo) string {
	var b strings.Builder
	if info.Flagged {
		b.WriteByte('F')
	}
	if info.Answered {
		b.WriteByte('R')
	}
	if info.Seen {
		b.WriteByte('S')
	}
	return b.String()
}

// expungeDue deletes from the server the folder's messages that are old
// enough under its policy, but only those whose local copy still verifies,
// only with deletion enabled, and only with UID EXPUNGE. Every deletion is
// journaled (and fsynced) before it happens.
func expungeDue(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	folder string, uidValidity uint32, policy config.Policy, dryRun bool, out *FolderSyncOut) error {
	cutoff := env.Now().Add(-time.Duration(policy.KeepDays) * 24 * time.Hour)
	due, err := ix.DueCopies(ctx, a.Name, folder, uidValidity, cutoff)
	if err != nil {
		return err
	}
	out.Due = len(due)
	switch {
	case len(due) == 0 || dryRun:
		return nil
	case !a.DeletionEnabled():
		out.Notes = append(out.Notes, "deletion is disabled for this account; nothing was deleted")
		return nil
	case !conn.HasUIDPlus():
		out.Notes = append(out.Notes, "the server does not support UIDPLUS; nothing was deleted")
		return nil
	}

	st, err := env.Store(a.Name)
	if err != nil {
		return err
	}
	var verified []index.ServerCopy
	var bad []string
	for _, c := range due {
		if err := st.Verify(c.Hash); err != nil {
			bad = append(bad, fmt.Sprintf("uid %d (%s: %v)", c.UID, c.Hash[:IDLen], err))
			continue
		}
		verified = append(verified, c)
	}
	if len(bad) > 0 {
		out.Notes = append(out.Notes, "local copy missing or damaged, kept on the server: "+strings.Join(bad, ", "))
	}
	if len(verified) == 0 {
		if len(bad) > 0 {
			return errors.New("no verified local copies to delete")
		}
		return nil
	}

	if err := journal.Append(env.Paths.Journal, journalEntries(env.Now(), journal.Intent, verified)); err != nil {
		return fmt.Errorf("journal (nothing was deleted): %w", err)
	}
	uids := make([]uint32, len(verified))
	for i, c := range verified {
		uids[i] = c.UID
	}
	if err := conn.Expunge(uids); err != nil {
		return err
	}
	if err := ix.MarkExpunged(ctx, verified, env.Now()); err != nil {
		return err
	}
	out.Expunged = len(verified)
	if err := journal.Append(env.Paths.Journal, journalEntries(env.Now(), journal.Done, verified)); err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("%d local copies failed verification", len(bad))
	}
	return nil
}

func journalEntries(at time.Time, phase string, copies []index.ServerCopy) []journal.Entry {
	entries := make([]journal.Entry, len(copies))
	for i, c := range copies {
		entries[i] = journal.Entry{
			Time: at.UTC(), Phase: phase, Account: c.Account, Folder: c.Folder,
			UIDValidity: c.UIDValidity, UID: c.UID, Hash: c.Hash,
		}
	}
	return entries
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
		conn, err := connect(ctx, env, a)
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
	},
}
