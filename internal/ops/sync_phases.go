// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/imapsync"
	"github.com/pluja/nought/internal/index"
)

// folderScan is what the first phase learned about one folder.
type folderScan struct {
	gone    []index.ServerCopy // known copies no longer on the server
	seen    []index.ServerCopy // dry run only: known mail among the new UIDs (not fetched)
	trusted bool               // the folder's state held (no UIDVALIDITY change, reused UID or held removal)
	held    bool               // a mass removal waits for confirmation
	refetch bool               // the folder is fetched again from the start next time
}

// syncPhases runs an account's sync:
//  1. per folder, archive new mail and find known copies that are gone;
//  2. apply what other clients did, only if every folder was trusted;
//  3. per folder, delete what is due;
//
// then empty known mail from the server's trash.
func syncPhases(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	in SyncIn, out *AccountSyncOut) error {
	folders, err := conn.Folders()
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(folders, func(f imapsync.Folder) bool { return isInbox(f.Name) }) {
		return errors.New("the server did not list INBOX; nothing was changed")
	}
	// INBOX first: a message another client moves out of it while this
	// sync runs is then either still seen in INBOX or seen in its new
	// folder, never missing from both.
	slices.SortStableFunc(folders, func(x, y imapsync.Folder) int { return cmp.Compare(rank(x), rank(y)) })

	scans, err := scanFolders(ctx, env, ix, conn, a, folders, in, out)
	if err != nil {
		return err
	}
	if err := applyOrDefer(ctx, env, ix, a.Name, scans, in.DryRun, out); err != nil {
		return err
	}
	if err := expungeFolders(ctx, env, ix, conn, a, folders, scans, in.DryRun, out); err != nil {
		return err
	}
	// The archive is safe whatever happens here: a failure is only a note.
	if a.DeletionEnabled() && !in.DryRun {
		if err := cleanServerTrash(ctx, env, ix, conn, a, folders, out); err != nil {
			out.Notes = append(out.Notes, "the server's trash was not emptied: "+err.Error())
		}
	}
	return nil
}

func rank(f imapsync.Folder) int {
	if isInbox(f.Name) {
		return 0
	}
	return 1
}

// scanFolders is phase 1. It returns one scan per folder (ignored folders
// get an untouched, trusted scan), plus one for folders that are gone from
// the server.
func scanFolders(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	folders []imapsync.Folder, in SyncIn, out *AccountSyncOut) ([]folderScan, error) {
	scans := make([]folderScan, 0, len(folders)+1)
	listed := map[string]bool{}
	for _, f := range folders {
		listed[f.Raw] = true
		policy := folderPolicy(a, f)
		fo := FolderSyncOut{Folder: f.Name, Action: policy.Action, KeepDays: policy.KeepDays, MaxDays: policy.MaxDays}
		scan := folderScan{trusted: true}
		if policy.Action != config.ActionIgnore {
			scan = scanFolder(ctx, env, ix, conn, a, f, in, &fo)
			out.BackInInbox += fo.BackInInbox
		}
		scans = append(scans, scan)
		out.Folders = append(out.Folders, fo)
		if in.Progress != nil {
			in.Progress(a.Name, fo)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	gone, err := vanishedFolders(ctx, ix, conn, a.Name, listed, in.AcceptRemovals, out)
	return append(scans, gone), err
}

// scanFolder archives a folder's new mail and finds the known copies that
// are gone.
func scanFolder(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	f imapsync.Folder, in SyncIn, out *FolderSyncOut) folderScan {
	scan, uidValidity, err := fetchFolder(ctx, env, ix, conn, a, f, in, out)
	if err == nil && !scan.refetch {
		scan.gone, scan.held, err = checkPresence(ctx, ix, conn, a.Name, f, uidValidity, in.AcceptRemovals, out)
		scan.trusted = scan.trusted && !scan.held
	}
	if errors.Is(err, imapsync.ErrIncompleteList) {
		// Often mail arriving or leaving during the sync: try next time.
		out.Notes = append(out.Notes, "the server's message list did not match its message count; "+
			"changes made on other devices are applied next time")
		return folderScan{}
	}
	if err != nil {
		out.Error = err.Error()
		return folderScan{}
	}
	return scan
}

// fetchFolder selects a folder and archives its new messages. State and
// server copies are keyed by the server's raw folder name, so folders whose
// display names collide never share state.
func fetchFolder(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	f imapsync.Folder, in SyncIn, out *FolderSyncOut) (folderScan, uint32, error) {
	scan := folderScan{trusted: true}
	mb, err := conn.Select(f.Raw, in.DryRun)
	if err != nil {
		return scan, 0, err
	}
	st, found, err := ix.SyncState(ctx, a.Name, f.Raw)
	if err != nil {
		return scan, 0, err
	}
	if !found || st.UIDValidity != mb.UIDValidity {
		if found {
			out.Notes = append(out.Notes, "UIDVALIDITY changed: the folder is fetched again and matched to the archive")
			scan.trusted = false
		}
		if !in.DryRun {
			if err := ix.ResetFolder(ctx, a.Name, f.Raw, mb.UIDValidity); err != nil {
				return scan, 0, err
			}
		}
		st = index.SyncState{UIDValidity: mb.UIDValidity}
	}
	uids, err := conn.UIDsAfter(st.LastUID)
	if err != nil {
		return scan, 0, err
	}
	out.New = len(uids)
	// Numbers only: enough to diagnose servers with unstable UIDs.
	env.Log.Debug("folder state", "account", a.Name, "folder", f.Name,
		"stored_found", found, "stored_uidvalidity", st.UIDValidity, "stored_last_uid", st.LastUID,
		"server_uidvalidity", mb.UIDValidity, "server_messages", mb.Messages,
		"new_uids", len(uids), "first_new_uid", firstOr(uids, 0), "last_new_uid", lastOr(uids, 0))
	if in.DryRun {
		if scan.trusted {
			scan.seen = previewMoves(ctx, env, ix, conn, a.Name, f, mb.UIDValidity, uids, out)
		}
		return scan, mb.UIDValidity, nil
	}
	// New UIDs after a completed sync are moves and deliveries; a fetch
	// from the start (new folder, reset) only finds known mail again.
	err = fetchNew(ctx, env, ix, conn, a.Name, f, uids, &st, st.Synced, out)
	if errors.Is(err, index.ErrCopyConflict) {
		// The server reused a UID for other content. Trust no UID in this
		// folder: forget its pending copies, delete nothing now, and fetch
		// it again from the start next time.
		out.Notes = append(out.Notes, "the server reused a message UID; nothing was deleted, and the folder will be fetched again")
		return folderScan{refetch: true}, mb.UIDValidity, ix.ForgetFolder(ctx, a.Name, f.Raw, st.UIDValidity)
	}
	return scan, mb.UIDValidity, err
}

// previewMoves is for dry runs, which fetch nothing: it recognises mail
// another client moved into the folder by its header, so the preview tells
// moves from deletes. With no server copies recorded yet (a first sync),
// nothing can have moved. A failure only makes the preview less precise.
func previewMoves(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, account string,
	f imapsync.Folder, uidValidity uint32, uids []uint32, out *FolderSyncOut) []index.ServerCopy {
	if st, err := ix.ServerStatus(ctx, account); err != nil || st.OnServer == 0 {
		return nil
	}
	var seen []index.ServerCopy
	for chunk := range slices.Chunk(uids, imapsync.MaxBatch) {
		known, err := knownMessages(ctx, env, ix, conn, account, f, uidValidity, chunk)
		if err != nil {
			out.Notes = append(out.Notes, "could not check for mail moved here; the preview may count it as deleted")
			return nil
		}
		seen = append(seen, known...)
	}
	return seen
}

// applyOrDefer is phase 2: what other clients did is applied only when
// every folder was trusted; otherwise nothing is recorded, and the next
// run finds the same changes again.
func applyOrDefer(ctx context.Context, env *Env, ix *index.Index, account string, scans []folderScan,
	dryRun bool, out *AccountSyncOut) error {
	var gone, seen []index.ServerCopy
	trusted := true
	for _, s := range scans {
		gone, seen = append(gone, s.gone...), append(seen, s.seen...)
		trusted = trusted && s.trusted
		out.RemovalsHeld = out.RemovalsHeld || s.held
	}
	switch {
	case trusted:
		return applyVanished(ctx, env, ix, account, gone, seen, dryRun, out)
	case out.RemovalsHeld:
		return nil // the held removal's note says what to do
	case len(gone) > 0:
		out.Notes = append(out.Notes, "some folders did not sync completely; "+
			"changes made on other devices are applied next time")
	}
	return nil
}

// expungeFolders is phase 3.
func expungeFolders(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	folders []imapsync.Folder, scans []folderScan, dryRun bool, out *AccountSyncOut) error {
	for i, f := range folders {
		fo := &out.Folders[i]
		if fo.Action == config.ActionIgnore || fo.Error != "" || scans[i].refetch {
			continue
		}
		if err := expungeFolder(ctx, env, ix, conn, a, f, dryRun, fo); err != nil {
			fo.Error = err.Error()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

// expungeFolder deletes the folder's due messages and records the sync.
func expungeFolder(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	f imapsync.Folder, dryRun bool, out *FolderSyncOut) error {
	mb, err := conn.Select(f.Raw, dryRun)
	if err != nil {
		return err
	}
	if err := expungeDue(ctx, env, ix, conn, a, f.Raw, mb.UIDValidity, folderPolicy(a, f), dryRun, out); err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	return ix.MarkSynced(ctx, a.Name, f.Raw, env.Now())
}
