// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/pluja/nought/internal/imapsync"
	"github.com/pluja/nought/internal/index"
	"github.com/pluja/nought/internal/store"
)

// fetchNew archives the given new UIDs, in chunks, recording progress after
// each message so an interrupted sync resumes where it stopped.
func fetchNew(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, account string, f imapsync.Folder,
	uids []uint32, st *index.SyncState, incremental bool, out *FolderSyncOut) error {
	for chunk := range slices.Chunk(uids, imapsync.MaxBatch) {
		infos, err := conn.Infos(chunk)
		if err != nil {
			return err
		}
		if err := fetchInfos(ctx, env, ix, conn, account, f, infos, st, incremental, out); err != nil {
			return err
		}
	}
	return nil
}

func fetchInfos(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, account string, f imapsync.Folder,
	infos []imapsync.MessageInfo, st *index.SyncState, incremental bool, out *FolderSyncOut) error {
	limit, err := maxMessageBytes(env)
	if err != nil {
		return err
	}
	for _, info := range infos {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fetchMessage(ctx, env, ix, conn, account, f, st.UIDValidity, info, incremental, limit, out)
		if note, ok := skipNote(err, info, limit); ok {
			// Remembered, so every sync tries it again and reports it
			// while it is there, and the cursor moves on to the rest.
			out.Notes = append(out.Notes, note)
			err = ix.RecordSkipped(ctx, account, f.Raw, st.UIDValidity, info.UID, info.Size)
		}
		if err != nil {
			return err
		}
		st.LastUID = info.UID
		if err := ix.SetSyncState(ctx, account, f.Raw, *st); err != nil {
			return err
		}
	}
	return nil
}

// retrySkipped tries again, at every sync, the folder's messages left on
// the server (too large, or not sent by it), and reports those still left.
// Ones gone from the server are forgotten. The cursor stays where it is.
func retrySkipped(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, account string,
	f imapsync.Folder, uidValidity uint32, out *FolderSyncOut) error {
	skipped, err := ix.Skipped(ctx, account, f.Raw, uidValidity)
	if err != nil || len(skipped) == 0 {
		return err
	}
	limit, err := maxMessageBytes(env)
	if err != nil {
		return err
	}
	uids := make([]uint32, len(skipped))
	for i, s := range skipped {
		uids[i] = s.UID
	}
	infos, err := conn.Lookup(uids)
	if err != nil {
		return err
	}
	for _, uid := range uids {
		info, ok := infos[uid]
		if ok {
			// Not incremental: the message was never archived, so it
			// cannot be a move back to the inbox.
			err = fetchMessage(ctx, env, ix, conn, account, f, uidValidity, info, false, limit, out)
			if note, ok := skipNote(err, info, limit); ok {
				out.Notes = append(out.Notes, note)
				continue
			}
			if err != nil {
				return err
			}
		}
		if err := ix.ForgetSkipped(ctx, account, f.Raw, uidValidity, uid); err != nil {
			return err
		}
	}
	return nil
}

// fetchMessage archives one message and records its server copy. It
// returns errTooLarge or errUnavailable, archiving nothing, for a message
// over the limit or one the server did not send.
func fetchMessage(ctx context.Context, env *Env, ix *index.Index, conn *imapsync.Conn, account string,
	f imapsync.Folder, uidValidity uint32, info imapsync.MessageInfo, incremental bool, limit int64, out *FolderSyncOut) error {
	raw, err := fetchOne(conn, info, limit)
	if err != nil {
		return err
	}
	// A new message gets the folder's tags. A message already archived
	// (seen in another folder, or fetched again after a reindex) only gains
	// the folder tag: re-adding "inbox" would undo triage, unless another
	// client moved it there.
	tags := []string{folderTag(f.Name)}
	if isInbox(f.Name) {
		tags = append(tags, TagInbox)
	}
	m, created, err := archive(ctx, env, account, raw, flagsOf(info), tags)
	if err != nil {
		return fmt.Errorf("archive uid %d: %w", info.UID, err)
	}
	if !created {
		if err := ix.ModifyTags(ctx, []int64{m.ID}, tags[:1], nil); err != nil { // idempotent
			return err
		}
		if incremental {
			back, err := foundOnServer(ctx, env, ix, account, m.Hash, isInbox(f.Name))
			if err != nil {
				return err
			}
			if back {
				out.BackInInbox++
			}
		}
	}
	out.Fetched++
	// Only a copy whose size matches what the server reports may ever be
	// deleted from the server.
	if !sizeMatches(raw, info.Size) {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"uid %d: the server reported %d bytes but sent %d; archived, but kept on the server", info.UID, info.Size, len(raw)))
		return nil
	}
	return ix.RecordCopy(ctx, index.ServerCopy{
		Account: account, Folder: f.Raw, FolderName: f.Name, UIDValidity: uidValidity, UID: info.UID,
		Hash: store.Hash(raw), Size: info.Size, InternalDate: info.InternalDate,
	}, env.Now())
}

var (
	errTooLarge = errors.New("larger than max_message_mb")
	// errUnavailable: the server did not send a message it lists (it
	// answered NO, sent no data, or an empty body).
	errUnavailable = errors.New("the server did not send it")
)

func fetchOne(conn *imapsync.Conn, info imapsync.MessageInfo, limit int64) ([]byte, error) {
	if info.Size > limit {
		return nil, errTooLarge
	}
	raw, err := conn.Fetch(info.UID, limit)
	switch {
	case errors.Is(err, imapsync.ErrTooLarge): // the server understated its size
		return nil, errTooLarge
	case errors.Is(err, imapsync.ErrUnavailable), errors.Is(err, imapsync.ErrGone), err == nil && len(raw) == 0:
		return nil, errUnavailable
	}
	return raw, err
}

// skipNote says why a message is left on the server, if err is one that
// leaves it there to be tried again (rather than a failed connection).
func skipNote(err error, info imapsync.MessageInfo, limit int64) (string, bool) {
	switch {
	case errors.Is(err, errTooLarge):
		return fmt.Sprintf("uid %d (%d MiB) is larger than max_message_mb (%d); left on the server until the limit allows it",
			info.UID, (info.Size+1<<20-1)>>20, limit>>20), true
	case errors.Is(err, errUnavailable):
		return fmt.Sprintf("uid %d: the server did not send it; left on the server, tried again at every sync", info.UID), true
	}
	return "", false
}

// maxMessageBytes is the largest message fetched: each is held in memory
// while it is archived.
func maxMessageBytes(env *Env) (int64, error) {
	cfg, err := env.Config()
	if err != nil {
		return 0, err
	}
	return cfg.MaxMessageBytes(), nil
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

// sizeMatches reports whether a fetched body plausibly is the whole message
// the server described. Servers that store LF line endings often report the
// LF size but send CRLF, so both forms are accepted.
func sizeMatches(raw []byte, size int64) bool {
	n := int64(len(raw))
	crlf := int64(bytes.Count(raw, []byte("\r\n")))
	lf := int64(bytes.Count(raw, []byte("\n")))
	return n == size || n-crlf == size || n+(lf-crlf) == size
}
