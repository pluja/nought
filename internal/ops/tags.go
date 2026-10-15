// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"nought/internal/atomicfile"
	"nought/internal/config"
	"nought/internal/index"
	"nought/internal/query"
	"nought/internal/store"
)

// TagsList lists every tag and how many messages carry it.
var TagsList = Op[struct{}, []index.TagCount]{
	Name:    "tags.list",
	Summary: "List tags and their message counts",
	Perm:    Read,
	Run: func(ctx context.Context, env *Env, _ struct{}) ([]index.TagCount, error) {
		ix, err := env.Index(ctx)
		if err != nil {
			return nil, err
		}
		return ix.TagCounts(ctx)
	},
}

// TagsDumpIn says where to write a dump. An empty Path returns the dump.
type TagsDumpIn struct {
	Path string `json:"path,omitempty"`
}

// TagsDumpOut is the dump, or where it was written.
type TagsDumpOut struct {
	Path string `json:"path,omitempty"`
	Dump string `json:"dump,omitempty"`
}

// TagsDump exports all tags in notmuch's dump format.
var TagsDump = Op[TagsDumpIn, TagsDumpOut]{
	Name:    "tags.dump",
	Summary: "Export all tags in notmuch dump format",
	Perm:    Read,
	Run: func(ctx context.Context, env *Env, in TagsDumpIn) (TagsDumpOut, error) {
		ix, err := env.Index(ctx)
		if err != nil {
			return TagsDumpOut{}, err
		}
		if in.Path == "" {
			var b bytes.Buffer
			err := ix.Dump(ctx, &b)
			return TagsDumpOut{Dump: b.String()}, err
		}
		err = atomicfile.Write(in.Path, 0o600, func(w io.Writer) error { return ix.Dump(ctx, w) })
		return TagsDumpOut{Path: in.Path}, err
	},
}

// TagsRestoreIn names the dump file to restore.
type TagsRestoreIn struct {
	Path string `json:"path"`
}

// TagsRestore sets tags from a dump (nought's or notmuch's).
var TagsRestore = Op[TagsRestoreIn, index.RestoreResult]{
	Name:    "tags.restore",
	Summary: "Set tags from a dump file (nought or notmuch format)",
	Perm:    Tag,
	Run: func(ctx context.Context, env *Env, in TagsRestoreIn) (index.RestoreResult, error) {
		f, err := os.Open(in.Path)
		if errors.Is(err, os.ErrNotExist) {
			return index.RestoreResult{}, fmt.Errorf("%s: %w", in.Path, ErrNotFound)
		}
		if err != nil {
			return index.RestoreResult{}, err
		}
		defer func() { _ = f.Close() }()
		ix, err := env.Index(ctx)
		if err != nil {
			return index.RestoreResult{}, err
		}
		res, err := ix.Restore(ctx, f)
		if err != nil {
			return res, usageError("restore %s: %v", in.Path, err)
		}
		if err := syncFlagsFromTags(ctx, env); err != nil {
			return res, err
		}
		return res, writeTagsDump(ctx, env)
	},
}

// ReindexOut summarizes a rebuild.
type ReindexOut struct {
	Accounts int                 `json:"accounts"`
	Messages int                 `json:"messages"`
	Restored index.RestoreResult `json:"restored"`
	Failed   int                 `json:"failed"`
	Failures []Failure           `json:"failures,omitempty"`
}

// IndexRebuild rebuilds the index from the Maildir files and the tag dump.
var IndexRebuild = Op[struct{}, ReindexOut]{
	Name:    "index.rebuild",
	Summary: "Rebuild the index from the archive files and the tag dump",
	Perm:    Tag,
	Run:     runReindex,
}

func runReindex(ctx context.Context, env *Env, _ struct{}) (ReindexOut, error) {
	var out ReindexOut
	// Refresh the dump from the current index first, so tags changed since
	// the last dump survive. A broken index keeps the previous dump.
	if _, err := os.Stat(env.Paths.IndexFile); err == nil {
		if err := writeTagsDump(ctx, env); err != nil {
			env.Log.Warn("could not refresh the tag dump from the old index; using the existing dump", "err", err)
		}
	}
	if err := env.closeIndex(); err != nil {
		return out, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(env.Paths.IndexFile + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return out, fmt.Errorf("remove old index: %w", err)
		}
	}

	accounts, err := archivedAccounts(env.Paths.ArchiveDir)
	if err != nil {
		return out, err
	}
	for _, account := range accounts {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		st, err := env.Store(account)
		if err != nil {
			return out, err
		}
		out.Accounts++
		for entry := range st.All() {
			raw, err := st.Get(entry.Hash)
			if err == nil {
				_, _, err = indexMessage(ctx, env, account, raw, entry.Flags, nil)
			}
			if err != nil {
				out.Failed++
				if len(out.Failures) < maxFailures {
					out.Failures = append(out.Failures, Failure{Source: account + "/" + entry.Hash[:12], Error: err.Error()})
				}
				continue
			}
			out.Messages++
		}
	}

	if f, err := os.Open(env.Paths.TagsDump); err == nil {
		ix, err := env.Index(ctx)
		if err != nil {
			return out, errors.Join(err, f.Close())
		}
		out.Restored, err = ix.Restore(ctx, f)
		if err := errors.Join(err, f.Close()); err != nil {
			return out, fmt.Errorf("restore tags: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return out, err
	}
	if err := writeTagsDump(ctx, env); err != nil {
		return out, err
	}
	if out.Failed > 0 {
		return out, fmt.Errorf("%d messages could not be indexed: %w", out.Failed, ErrPartial)
	}
	return out, nil
}

// archivedAccounts lists account directories in the archive.
func archivedAccounts(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && config.ValidateAccountName(e.Name()) == nil {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// syncFlagsFromTags brings Maildir flags in line with unread/flagged/replied
// tags after a bulk change such as a restore.
func syncFlagsFromTags(ctx context.Context, env *Env) error {
	ix, err := env.Index(ctx)
	if err != nil {
		return err
	}
	return ix.Each(ctx, query.Query{}, func(page []index.Message) error {
		return applyFlags(env, page)
	})
}

// applyFlags renames message files so their flags match their tags.
func applyFlags(env *Env, msgs []index.Message) error {
	var errs []error
	for _, m := range msgs {
		st, err := env.Store(m.Account)
		if err != nil {
			return err
		}
		current, err := st.Flags(m.Hash)
		if errors.Is(err, store.ErrNotFound) {
			continue // indexed but file missing; reported by verification elsewhere
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if want := tagFlags(current, m.Tags); want != current {
			if err := st.SetFlags(m.Hash, want); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", m.Hash[:12], err))
			}
		}
	}
	return errors.Join(errs...)
}
