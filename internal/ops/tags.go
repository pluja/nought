// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/pluja/nought/internal/atomicfile"
	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/index"
	"github.com/pluja/nought/internal/query"
	"github.com/pluja/nought/internal/store"
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
	// Trash includes deleted and expiring mail, which is left out by
	// default so a dump made for a backup never names it.
	Trash bool `json:"trash,omitempty"`
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
		keep := func(tags []string) bool { return in.Trash || !isTransient(tags) }
		if in.Path == "" {
			var b bytes.Buffer
			err := ix.Dump(ctx, &b, keep)
			return TagsDumpOut{Dump: b.String()}, err
		}
		err = atomicfile.Write(in.Path, 0o600, func(w io.Writer) error { return ix.Dump(ctx, w, keep) })
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
	release, err := acquireLock(env)
	if err != nil {
		return out, err
	}
	defer release()
	if err := refreshDumpBeforeReindex(ctx, env); err != nil {
		return out, err
	}

	// Build the new index beside the old one and swap it in only when it is
	// complete, so an interrupted reindex never leaves a partial index (whose
	// dump would lose tags) in place.
	next := env.Paths.IndexFile + ".new"
	if err := removeDB(next); err != nil {
		return out, err
	}
	ix, err := index.Open(ctx, next)
	if err != nil {
		return out, err
	}
	built := false
	defer func() {
		if !built {
			_ = ix.Close()
			_ = removeDB(next)
		}
	}()

	accounts, err := archivedAccounts(env.Paths.ArchiveDir, env.Paths.TransientDir)
	if err != nil {
		return out, err
	}
	for _, account := range accounts {
		out.Accounts++
		for _, open := range []func(string) (*store.Maildir, error){env.Store, env.Transient} {
			st, err := open(account)
			if err != nil {
				return out, err
			}
			for entry := range st.All() {
				if err := ctx.Err(); err != nil {
					return out, err
				}
				raw, err := st.Get(entry.Hash)
				if err == nil {
					_, _, err = indexMessage(ctx, env, ix, account, raw, entry.Flags, nil)
				}
				if err != nil {
					out.Failed++
					if len(out.Failures) < maxFailures {
						out.Failures = append(out.Failures, Failure{Source: account + "/" + entry.Hash[:IDLen], Error: err.Error()})
					}
					continue
				}
				out.Messages++
			}
		}
	}
	for _, dump := range []string{env.Paths.TagsDump, env.Paths.TransientDump} {
		res, err := restoreDumpFile(ctx, ix, dump)
		if err != nil {
			return out, err
		}
		out.Restored.Applied += res.Applied
		out.Restored.Missing += res.Missing
		out.Restored.SkippedTags += res.SkippedTags
	}

	// Swap: close both, drop the old database and its WAL files, rename.
	if err := ix.Close(); err != nil {
		return out, err
	}
	built = true
	if err := env.closeIndex(); err != nil {
		return out, err
	}
	if err := removeDB(env.Paths.IndexFile); err != nil {
		return out, err
	}
	if err := os.Rename(next, env.Paths.IndexFile); err != nil {
		return out, fmt.Errorf("install new index: %w", err)
	}
	if err := atomicfile.SyncDir(filepath.Dir(env.Paths.IndexFile)); err != nil {
		return out, err
	}

	if err := syncFlagsFromTags(ctx, env); err != nil {
		return out, err
	}
	// The dumps are the truth for tags: put every file where its tags say
	// (an interrupted move can leave mail in the wrong store).
	if err := placeAll(ctx, env); err != nil {
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

// refreshDumpBeforeReindex writes the dump from the current index so tag
// changes since the last dump survive. If the index cannot be opened (it is
// what is being repaired), the existing dump is used. If it opens but the
// dump cannot be written, reindexing stops: continuing would lose tags.
func refreshDumpBeforeReindex(ctx context.Context, env *Env) error {
	if _, err := os.Stat(env.Paths.IndexFile); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if _, err := env.Index(ctx); err != nil {
		env.Log.Warn("the current index cannot be opened; rebuilding from the existing tag dump", "err", err)
		return nil
	}
	if err := writeTagsDump(ctx, env); err != nil {
		return fmt.Errorf("refresh tag dump before reindex (nothing was changed): %w", err)
	}
	return nil
}

func restoreDumpFile(ctx context.Context, ix *index.Index, path string) (index.RestoreResult, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return index.RestoreResult{}, nil
	}
	if err != nil {
		return index.RestoreResult{}, err
	}
	res, err := ix.Restore(ctx, f)
	if err := errors.Join(err, f.Close()); err != nil {
		return res, fmt.Errorf("restore tags from %s: %w", path, err)
	}
	return res, nil
}

// removeDB removes an SQLite database and its WAL and shared-memory files.
func removeDB(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", filepath.Base(path+suffix), err)
		}
	}
	return nil
}

// archivedAccounts lists account directories in the archive.
func archivedAccounts(dirs ...string) ([]string, error) {
	var names []string
	for _, dir := range dirs {
		found, err := accountDirs(dir)
		if err != nil {
			return nil, err
		}
		for _, n := range found {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	return names, nil
}

func accountDirs(dir string) ([]string, error) {
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
		st, err := env.locate(m.Account, m.Hash)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		current, err := st.Flags(m.Hash)
		if errors.Is(err, store.ErrNotFound) {
			continue // indexed but file missing; reported by verification elsewhere
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		want, err := store.NormalizeFlags(tagFlags(current, m.Tags))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if want != current {
			if err := st.SetFlags(m.Hash, want); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", m.Hash[:IDLen], err))
			}
		}
	}
	return errors.Join(errs...)
}
