// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"path"
	"slices"
	"strings"

	"nought/internal/config"
	"nought/internal/imapsync"
	"nought/internal/index"
	"nought/internal/mailmsg"
	"nought/internal/query"
)

// serverTrash finds the server's trash folder: the one marked \Trash, or
// one with a trash-like name. Only an ignored trash is cleaned here; an
// archived one is handled like any other folder.
func serverTrash(a *config.Account, folders []imapsync.Folder) (imapsync.Folder, bool) {
	i := slices.IndexFunc(folders, func(f imapsync.Folder) bool { return f.Trash })
	if i < 0 {
		i = slices.IndexFunc(folders, func(f imapsync.Folder) bool { return trashName(f.Name) })
	}
	if i < 0 || a.Policy(folders[i].Name).Action != config.ActionIgnore {
		return imapsync.Folder{}, false
	}
	return folders[i], true
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
		known, err := knownInTrash(ctx, env, ix, conn, a.Name, trash, mb.UIDValidity, chunk)
		if err != nil {
			return err
		}
		if len(known) > 0 {
			private := map[uint32]bool{}
			for _, c := range known {
				private[c.UID] = true
			}
			if err := expungeBatch(ctx, env, ix, conn, known, private); err != nil {
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

// knownInTrash returns the trash's messages (among uids) that are archived
// here: same Message-ID, same header block, a matching size, and a local
// copy that still verifies. No message body is downloaded.
func knownInTrash(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, account string,
	trash imapsync.Folder, uidValidity uint32, uids []uint32) ([]index.ServerCopy, error) {
	infos, err := conn.Lookup(uids)
	if err != nil {
		return nil, err
	}
	headers, err := conn.Headers(uids)
	if err != nil {
		return nil, err
	}
	var known []index.ServerCopy
	for _, uid := range uids {
		info, ok := infos[uid]
		h, _ := mailmsg.Parse(headers[uid]) // the header block alone
		if !ok || h.MessageID == "" {
			continue
		}
		candidates, err := ix.Search(ctx, query.Query{Groups: [][]query.Term{
			{{Field: query.ID, Value: h.MessageID}}, {{Field: query.Account, Value: account}},
		}}, index.MaxLimit, 0)
		if err != nil {
			return nil, err
		}
		for _, m := range candidates {
			raw, err := verifiedLocal(env, account, m.Hash)
			if err == nil && sameHeader(headers[uid], raw) && sizeMatches(raw, info.Size) {
				known = append(known, index.ServerCopy{Account: account, Folder: trash.Raw, FolderName: trash.Name,
					UIDValidity: uidValidity, UID: uid, Hash: m.Hash, Size: info.Size})
				break
			}
		}
	}
	return known, nil
}
