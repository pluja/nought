// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"nought/internal/config"
	"nought/internal/imapsync"
	"nought/internal/index"
	"nought/internal/journal"
	"nought/internal/store"
)

// expungeDue deletes from the server the folder's messages that are due:
// handled (out of the inbox) and kept at least keep_days, or older than
// max_days. It deletes only those whose local copy still verifies and
// whose server UID still shows the same message, only with deletion
// enabled, and only with UID EXPUNGE. Every deletion is journaled (and
// fsynced) before it happens.
func expungeDue(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, a *config.Account,
	folder string, uidValidity uint32, policy config.Policy, dryRun bool, out *FolderSyncOut) error {
	due, err := ix.HandledCopies(ctx, a.Name, folder, uidValidity, env.Now().Add(-days(policy.KeepDays)))
	if err != nil {
		return err
	}
	if policy.MaxDays > 0 {
		expired, err := ix.DueCopies(ctx, a.Name, folder, uidValidity, env.Now().Add(-days(policy.MaxDays)))
		if err != nil {
			return err
		}
		due = unionByUID(due, expired)
	}
	// Messages the user deleted (or that expired) leave the server whatever
	// the folder's keep_days or the account's automatic deletion setting.
	requested, err := ix.DeletionRequested(ctx, a.Name, folder, uidValidity, env.Now().UTC().Format(dateLayout))
	if err != nil {
		return err
	}
	out.Due = len(due)
	if !a.DeletionEnabled() {
		if len(due) > 0 && len(requested) == 0 {
			out.Notes = append(out.Notes, "deletion is disabled for this account; nothing was deleted")
		}
		due = nil
	}
	due = unionByUID(due, requested)
	// The journal is backed up: for mail the user deleted it records
	// neither the content hash nor the Message-ID.
	private := map[uint32]bool{}
	for _, r := range requested {
		private[r.UID] = true
	}
	switch {
	case len(due) == 0 || dryRun:
		return nil
	case !conn.HasUIDPlus():
		out.Notes = append(out.Notes, "the server does not support UIDPLUS; nothing was deleted")
		return nil
	}

	var failed int
	for batch := range slices.Chunk(due, imapsync.MaxBatch) {
		if err := ctx.Err(); err != nil {
			return err
		}
		verified, gone, bad, err := checkBeforeExpunge(env, conn, a.Name, batch)
		if err != nil {
			return err
		}
		failed += len(bad)
		if len(bad) > 0 {
			out.Notes = append(out.Notes, "kept on the server: "+strings.Join(bad, "; "))
		}
		// Already gone from the server (deleted elsewhere): just bookkeeping.
		if err := ix.MarkExpunged(context.WithoutCancel(ctx), gone, env.Now()); err != nil {
			return err
		}
		if len(verified) == 0 {
			continue
		}
		if err := expungeBatch(ctx, env, ix, conn, verified, private); err != nil {
			return err
		}
		out.Expunged += len(verified)
	}
	if failed > 0 {
		return fmt.Errorf("%d messages could not be verified and were kept on the server", failed)
	}
	return nil
}

// checkBeforeExpunge splits a batch into copies safe to delete, copies
// already gone from the server, and copies that must stay (described).
func checkBeforeExpunge(env *Env, conn *imapsync.Conn, account string,
	batch []index.ServerCopy) (verified, gone []index.ServerCopy, bad []string, err error) {
	uids := make([]uint32, len(batch))
	for i, c := range batch {
		uids[i] = c.UID
	}
	onServer, err := conn.Lookup(uids)
	if err != nil {
		return nil, nil, nil, err
	}
	// The message under each UID must still be the archived one: its header
	// block is compared with the local copy's. UIDs alone are not trusted,
	// because some servers renumber or reuse them.
	headers, err := conn.Headers(uids)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, c := range batch {
		info, ok := onServer[c.UID]
		switch {
		case !ok:
			gone = append(gone, c)
		case info.Size != c.Size:
			bad = append(bad, fmt.Sprintf("uid %d changed on the server", c.UID))
		default:
			raw, err := verifiedLocal(env, account, c.Hash)
			if err != nil {
				bad = append(bad, fmt.Sprintf("uid %d: local copy %s %v", c.UID, c.Hash[:IDLen], err))
				continue
			}
			if !sameHeader(headers[c.UID], raw) {
				bad = append(bad, fmt.Sprintf("uid %d: the message on the server is not the archived one", c.UID))
				continue
			}
			verified = append(verified, c)
		}
	}
	return verified, gone, bad, nil
}

// verifiedLocal reads a message's local copy and checks it still matches
// its hash.
func verifiedLocal(env *Env, account, hash string) ([]byte, error) {
	st, err := env.locate(account, hash)
	if err != nil {
		return nil, err
	}
	raw, err := st.Get(hash)
	if err == nil && store.Hash(raw) != hash {
		err = store.ErrCorrupt
	}
	return raw, err
}

// sameHeader reports whether a header block fetched from the server is the
// header of raw, ignoring line-ending differences (servers that store LF
// send CRLF).
func sameHeader(serverHeader, raw []byte) bool {
	if len(serverHeader) == 0 {
		return false
	}
	norm := func(b []byte) []byte { return bytes.TrimRight(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")), "\n") }
	local := norm(raw)
	if i := bytes.Index(local, []byte("\n\n")); i >= 0 {
		local = local[:i]
	}
	return bytes.Equal(norm(serverHeader), local)
}

// expungeBatch journals, deletes, and records one batch. Once the server
// has deleted the messages, the bookkeeping finishes even if the user
// interrupts, so the journal never shows an intent without its outcome.
func expungeBatch(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, batch []index.ServerCopy,
	private map[uint32]bool) error {
	if err := journal.Append(env.Paths.Journal, journalEntries(ctx, ix, env.Now(), journal.Intent, batch, private)); err != nil {
		return fmt.Errorf("journal (nothing was deleted): %w", err)
	}
	uids := make([]uint32, len(batch))
	for i, c := range batch {
		uids[i] = c.UID
	}
	if err := conn.Expunge(uids); err != nil {
		return err
	}
	done := context.WithoutCancel(ctx)
	if err := ix.MarkExpunged(done, batch, env.Now()); err != nil {
		return err
	}
	return journal.Append(env.Paths.Journal, journalEntries(done, ix, env.Now(), journal.Done, batch, private))
}

func journalEntries(ctx context.Context, ix *index.Index, at time.Time, phase string, copies []index.ServerCopy,
	private map[uint32]bool) []journal.Entry {
	entries := make([]journal.Entry, len(copies))
	for i, c := range copies {
		entries[i] = journal.Entry{
			Time: at.UTC(), Phase: phase, Account: c.Account, Folder: c.Folder,
			UIDValidity: c.UIDValidity, UID: c.UID,
		}
		if !private[c.UID] {
			entries[i].Hash = c.Hash
			entries[i].MessageID = ix.MessageIDOf(ctx, c.Account, c.Hash)
		}
	}
	return entries
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// unionByUID merges two lists of one folder's copies, in UID order.
func unionByUID(a, b []index.ServerCopy) []index.ServerCopy {
	out := slices.Clone(a)
	for _, c := range b {
		if !slices.ContainsFunc(out, func(x index.ServerCopy) bool { return x.UID == c.UID }) {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(x, y index.ServerCopy) int { return cmp.Compare(x.UID, y.UID) })
	return out
}
