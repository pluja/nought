// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"nought/internal/imapsync"
	"nought/internal/index"
	"nought/internal/store"
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
	tags := []string{folderTag(f.Name)}
	if isInbox(f.Name) {
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
			// A new message gets the folder's tags. A message already
			// archived (seen in another folder, or fetched again after a
			// reindex) only gains the folder tag: re-adding "inbox" would
			// undo triage. Unless another client moved it back to the INBOX.
			m, created, err := archive(ctx, env, account, raw, flagsOf(info), tags)
			if err != nil {
				return fmt.Errorf("archive uid %d: %w", info.UID, err)
			}
			if !created {
				if err := ix.ModifyTags(ctx, []int64{m.ID}, tags[:1], nil); err != nil { // idempotent
					return err
				}
				if incremental && isInbox(f.Name) {
					back, err := backToInbox(ctx, env, ix, account, m.Hash)
					if err != nil {
						return err
					}
					if back {
						out.BackInInbox++
					}
				}
			}
			out.Fetched++
			// Only a copy whose size matches what the server reports may
			// ever be deleted from the server.
			if !sizeMatches(raw, info.Size) {
				out.Notes = append(out.Notes, fmt.Sprintf(
					"uid %d: the server reported %d bytes but sent %d; archived, but kept on the server", info.UID, info.Size, len(raw)))
				break
			}
			if err := ix.RecordCopy(ctx, index.ServerCopy{
				Account: account, Folder: f.Raw, FolderName: f.Name, UIDValidity: st.UIDValidity, UID: info.UID,
				Hash: store.Hash(raw), Size: info.Size, InternalDate: info.InternalDate,
			}, env.Now()); err != nil {
				return err
			}
		}
		st.LastUID = info.UID
		if err := ix.SetSyncState(ctx, account, f.Raw, *st); err != nil {
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
	raw, err := conn.Fetch(info.UID, maxMessageSize)
	if errors.Is(err, imapsync.ErrTooLarge) {
		return nil, fmt.Errorf("%w: larger than %d MiB", errSkip, maxMessageSize>>20)
	}
	if errors.Is(err, imapsync.ErrGone) {
		return nil, fmt.Errorf("%w: deleted on the server meanwhile", errSkip)
	}
	if err == nil && len(raw) == 0 {
		return nil, fmt.Errorf("%w: the server sent an empty message", errSkip)
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

// sizeMatches reports whether a fetched body plausibly is the whole message
// the server described. Servers that store LF line endings often report the
// LF size but send CRLF, so both forms are accepted.
func sizeMatches(raw []byte, size int64) bool {
	n := int64(len(raw))
	crlf := int64(bytes.Count(raw, []byte("\r\n")))
	lf := int64(bytes.Count(raw, []byte("\n")))
	return n == size || n-crlf == size || n+(lf-crlf) == size
}
