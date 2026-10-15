// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"nought/internal/imapsync"
	"nought/internal/index"
)

// The server's INBOX mirrors nought's inbox: what another client (a phone)
// did to a message is applied here too. A message whose INBOX copy vanished
// was archived elsewhere if a copy remains in another folder, and deleted
// elsewhere if none remains. See docs/superpowers/specs/2026-10-15-inbox-mirror-design.md.

// massRemovalMin and the half-of-the-folder rule decide when removals by
// another client look like a server glitch rather than a person's choice.
const massRemovalMin = 20

// minTrashDays keeps mail deleted on another client in the trash at least
// this long, whatever trash_days says: a margin in case a deletion was
// misread.
const minTrashDays = 7

// checkPresence finds the folder's live copies that are no longer on the
// server. held reports a mass removal that was not accepted: then none are
// returned. Nothing is recorded here; see applyVanished.
func checkPresence(ctx context.Context, ix *index.Index, conn *imapsync.Conn, account string,
	f imapsync.Folder, uidValidity uint32, accept bool, out *FolderSyncOut) (gone []index.ServerCopy, held bool, err error) {
	present, err := conn.AllUIDs()
	if err != nil {
		return nil, false, err
	}
	live, err := ix.LiveCopies(ctx, account, f.Raw, uidValidity)
	if err != nil {
		return nil, false, err
	}
	for _, c := range live {
		if _, ok := slices.BinarySearch(present, c.UID); !ok {
			gone = append(gone, c)
		}
	}
	out.Vanished = len(gone)
	if note := holdMassRemoval(gone, len(live), accept); note != "" {
		out.Notes = append(out.Notes, note)
		return nil, true, nil
	}
	return gone, false, nil
}

// vanishedFolders finds folders that have live copies but are no longer on
// the server (deleted on another client): all their copies vanished.
func vanishedFolders(ctx context.Context, ix *index.Index, account string, listed map[string]bool,
	accept bool, out *AccountSyncOut) (gone []index.ServerCopy, held bool, err error) {
	tracked, err := ix.TrackedFolders(ctx, account)
	if err != nil {
		return nil, false, err
	}
	for folder, validity := range tracked {
		if listed[folder] {
			continue
		}
		live, err := ix.LiveCopies(ctx, account, folder, validity)
		if err != nil {
			return nil, false, err
		}
		if note := holdMassRemoval(live, len(live), accept); note != "" {
			out.Notes, held = append(out.Notes, note), true
			continue
		}
		gone = append(gone, live...)
	}
	return gone, held, nil
}

// holdMassRemoval returns a note if many messages disappeared from a folder
// at once and that was not accepted: a server glitch must never look like
// "the user deleted everything".
func holdMassRemoval(gone []index.ServerCopy, live int, accept bool) string {
	if accept || len(gone) <= massRemovalMin || len(gone)*2 <= live {
		return ""
	}
	return fmt.Sprintf("%d of %d messages in %s disappeared from the server at once; nothing was changed. "+
		"If that was you, sync with accept-removals", len(gone), live, gone[0].FolderName)
}

// applyVanished applies what other clients did, then records the vanished
// copies. It runs only after a clean first phase, so a removal is never
// recorded without being applied: an incomplete run changes nothing, and
// the next one finds the same removals again.
func applyVanished(ctx context.Context, env *Env, ix *index.Index, account string,
	vanished []index.ServerCopy, dryRun bool, out *AccountSyncOut) error {
	if err := classifyVanished(ctx, env, ix, account, vanished, dryRun, out); err != nil || dryRun {
		return err
	}
	return ix.MarkVanished(ctx, vanished, env.Now())
}

// classifyVanished applies what another client did to the messages whose
// INBOX copy vanished. Messages already handled here are left alone.
func classifyVanished(ctx context.Context, env *Env, ix *index.Index, account string,
	vanished []index.ServerCopy, dryRun bool, out *AccountSyncOut) error {
	var hashes []string
	for _, c := range vanished {
		if isInbox(c.FolderName) {
			hashes = append(hashes, c.Hash)
		}
	}
	if len(hashes) == 0 {
		return nil
	}
	slices.Sort(hashes)
	hashes = slices.Compact(hashes)
	on, err := ix.OnServer(ctx, account, hashes)
	if err != nil {
		return err
	}
	cfg, err := env.Config()
	if err != nil {
		return err
	}
	expiry := deleteAfterTag(env.Now().AddDate(0, 0, max(cfg.TrashKeepDays(), minTrashDays)))
	for _, h := range hashes {
		// The vanished copies are not recorded yet: leave them out.
		copies := slices.DeleteFunc(on[h], func(c index.ServerCopy) bool { return isVanished(c, vanished) })
		if slices.ContainsFunc(copies, func(c index.ServerCopy) bool { return isInbox(c.FolderName) }) {
			continue // still in the server's INBOX (e.g. a duplicate delivery)
		}
		in := TagIn{Query: "hash:" + h + " account:" + account + " tag:" + TagInbox, Remove: []string{TagInbox}, DryRun: dryRun}
		count := &out.ArchivedElsewhere
		if len(copies) == 0 {
			in.Add, count = []string{TagDeleted, expiry}, &out.DeletedElsewhere
		}
		res, err := runTag(ctx, env, in)
		if err != nil {
			return err
		}
		*count += res.Matched
	}
	return nil
}

// backToInbox handles a known message that appeared under a new INBOX UID:
// another client moved it back to the inbox, so it returns here too, out of
// the trash if it was there.
func backToInbox(ctx context.Context, env *Env, ix *index.Index, account, hash string) (bool, error) {
	m, err := ix.Resolve(ctx, account, hash)
	if err != nil {
		return false, err
	}
	remove := slices.DeleteFunc(slices.Clone(m.Tags), func(t string) bool { return !IsDeletionTag(t) })
	if slices.Contains(m.Tags, TagInbox) && len(remove) == 0 {
		return false, nil
	}
	_, err = runTag(ctx, env, TagIn{Query: "hash:" + hash + " account:" + account, Add: []string{TagInbox}, Remove: remove})
	return err == nil, err
}

func isInbox(folder string) bool { return strings.EqualFold(folder, "INBOX") }

func isVanished(c index.ServerCopy, vanished []index.ServerCopy) bool {
	return slices.ContainsFunc(vanished, func(v index.ServerCopy) bool {
		return v.Folder == c.Folder && v.UIDValidity == c.UIDValidity && v.UID == c.UID
	})
}
