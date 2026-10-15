// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"nought/internal/config"
	"nought/internal/imapsync"
	"nought/internal/index"
	"nought/internal/journal"
)

// expungeDue deletes from the server the folder's messages that are old
// enough under its policy, but only those whose local copy still verifies
// and whose server UID still shows the same size and date, only with
// deletion enabled, and only with UID EXPUNGE. Every deletion is journaled
// (and fsynced) before it happens.
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
		if err := expungeBatch(ctx, env, ix, conn, verified); err != nil {
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
	st, err := env.Store(account)
	if err != nil {
		return nil, nil, nil, err
	}
	uids := make([]uint32, len(batch))
	for i, c := range batch {
		uids[i] = c.UID
	}
	onServer, err := conn.Lookup(uids)
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
			if err := st.Verify(c.Hash); err != nil {
				bad = append(bad, fmt.Sprintf("uid %d: local copy %s %v", c.UID, c.Hash[:IDLen], err))
				continue
			}
			verified = append(verified, c)
		}
	}
	return verified, gone, bad, nil
}

// expungeBatch journals, deletes, and records one batch. Once the server
// has deleted the messages, the bookkeeping finishes even if the user
// interrupts, so the journal never shows an intent without its outcome.
func expungeBatch(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, batch []index.ServerCopy) error {
	if err := journal.Append(env.Paths.Journal, journalEntries(ctx, ix, env.Now(), journal.Intent, batch)); err != nil {
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
	return journal.Append(env.Paths.Journal, journalEntries(done, ix, env.Now(), journal.Done, batch))
}

func journalEntries(ctx context.Context, ix *index.Index, at time.Time, phase string, copies []index.ServerCopy) []journal.Entry {
	entries := make([]journal.Entry, len(copies))
	for i, c := range copies {
		entries[i] = journal.Entry{
			Time: at.UTC(), Phase: phase, Account: c.Account, Folder: c.Folder,
			UIDValidity: c.UIDValidity, UID: c.UID, MessageID: ix.MessageIDOf(ctx, c.Account, c.Hash), Hash: c.Hash,
		}
	}
	return entries
}
