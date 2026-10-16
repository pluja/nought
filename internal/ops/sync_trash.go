// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"path"
	"slices"
	"strings"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/imapsync"
	"github.com/pluja/nought/internal/index"
)

// serverTrash finds the server's trash folder: the one marked \Trash, or
// one with a trash-like name. Only an ignored trash is cleaned here; an
// archived one is handled like any other folder.
func serverTrash(a *config.Account, folders []imapsync.Folder) (imapsync.Folder, bool) {
	i := slices.IndexFunc(folders, func(f imapsync.Folder) bool { return f.Trash })
	if i < 0 {
		i = slices.IndexFunc(folders, func(f imapsync.Folder) bool { return trashName(f.Name) })
	}
	if i < 0 || folderPolicy(a, folders[i]).Action != config.ActionIgnore {
		return imapsync.Folder{}, false
	}
	return folders[i], true
}

// folderPolicy is what sync does with a folder: its rule, if one names it;
// else nothing for the folders the server marks as trash, drafts, junk, all
// mail or flagged, whatever their name in the server's language; else the
// default (archive).
func folderPolicy(a *config.Account, f imapsync.Folder) config.Policy {
	if p, ok := a.Rule(f.Name); ok {
		return p
	}
	if f.Trash || f.Special {
		return config.FolderRule{Action: config.ActionIgnore}.Policy()
	}
	return a.Policy(f.Name)
}

// trashName reports names (the last path element, any case) taken for the
// server's trash when it marks none with SPECIAL-USE.
func trashName(folder string) bool {
	switch strings.ToLower(path.Base(folder)) {
	case "trash", "deleted", "deleted items", "deleted messages":
		return true
	}
	return false
}

// trashCursor is where the cleanup remembers the last trash UID it looked
// at: its own sync-state key (NUL cannot occur in IMAP folder names), so it
// never moves the folder's own place should the trash be archived later.
func trashCursor(raw string) string { return "\x00trash-cleanup:" + raw }

// cleanServerTrash deletes from the server's trash the messages nought has
// already archived: a phone usually deletes by moving mail there. Unknown
// messages are left alone, because nought never deletes what it has not
// saved. Each message is examined once (trashCursor), and deletions take
// the usual journaled path.
func cleanServerTrash(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	folders []imapsync.Folder, out *AccountSyncOut) error {
	trash, ok := serverTrash(a, folders)
	if !ok || !conn.HasUIDPlus() {
		return nil
	}
	mb, err := conn.Select(trash.Raw, false)
	if err != nil {
		return err
	}
	st, found, err := ix.SyncState(ctx, a.Name, trashCursor(trash.Raw))
	if err != nil {
		return err
	}
	if !found || st.UIDValidity != mb.UIDValidity {
		st = index.SyncState{UIDValidity: mb.UIDValidity}
	}
	uids, err := conn.UIDsAfter(st.LastUID)
	if err != nil {
		return err
	}
	for chunk := range slices.Chunk(uids, imapsync.MaxBatch) {
		known, err := knownMessages(ctx, env, ix, conn, a.Name, trash, mb.UIDValidity, chunk)
		if err != nil {
			return err
		}
		if len(known) > 0 {
			if err := expungeBatch(ctx, env, ix, conn, known, uidSet(known)); err != nil {
				return err
			}
			out.TrashCleaned += len(known)
		}
		st.LastUID = chunk[len(chunk)-1]
		if err := ix.SetSyncState(ctx, a.Name, trashCursor(trash.Raw), st); err != nil {
			return err
		}
	}
	return nil
}
