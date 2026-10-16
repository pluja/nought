// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/pluja/nought/internal/atomicfile"
	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/index"
	"github.com/pluja/nought/internal/query"
	"github.com/pluja/nought/internal/store"
)

// The index is a cache: everything in it can be rebuilt from the archive
// files and the tag dumps. The dumps are the only copy of the tags, so an
// index that does not reflect the archive (missing, empty, half-built) must
// never be written over them.

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
	release, err := acquireLock(env)
	if err != nil {
		return ReindexOut{}, err
	}
	defer release()
	if err := refreshDumpBeforeReindex(ctx, env); err != nil {
		return ReindexOut{}, err
	}
	// Built beside the old index and swapped in only when complete, so an
	// interrupted reindex never leaves a partial index in place.
	next := env.Paths.IndexFile + ".new"
	out, err := buildIndex(ctx, env, next)
	if err != nil {
		return out, err
	}
	if err := env.closeIndex(); err != nil {
		return out, err
	}
	if err := installIndex(next, env.Paths.IndexFile); err != nil {
		return out, err
	}
	if err := settle(ctx, env); err != nil {
		return out, err
	}
	if out.Failed > 0 {
		return out, fmt.Errorf("%d messages could not be indexed: %w", out.Failed, ErrPartial)
	}
	return out, nil
}

// settle makes the files agree with the new index: flags from tags, every
// file in the store its tags say (an interrupted move can leave mail in the
// wrong one), and a fresh dump.
func settle(ctx context.Context, env *Env) error {
	env.dropStores() // the archive is scanned again, as the index just was
	if err := syncFlagsFromTags(ctx, env); err != nil {
		return err
	}
	if err := placeAll(ctx, env); err != nil {
		return err
	}
	return writeTagsDump(ctx, env)
}

// buildIndex builds a complete index at path from the archive and the dumps.
// It reads the archive afresh: another nou may have archived mail since this
// one last looked. On failure nothing is left at path.
func buildIndex(ctx context.Context, env *Env, path string) (out ReindexOut, err error) {
	if err := removeDB(path); err != nil {
		return out, err
	}
	ix, err := index.Open(ctx, path)
	if err != nil {
		return out, err
	}
	defer func() {
		err = errors.Join(err, ix.Close())
		if err != nil {
			_ = removeDB(path)
		}
	}()
	accounts, err := archivedAccounts(env.Paths.ArchiveDir, env.Paths.TransientDir)
	if err != nil {
		return out, err
	}
	for _, account := range accounts {
		out.Accounts++
		for _, dir := range []string{env.Paths.AccountArchive(account), env.Paths.TransientArchive(account)} {
			if err := indexStore(ctx, env, ix, account, dir, &out); err != nil {
				return out, err
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
	return out, nil
}

// indexStore indexes every message of one Maildir; a message that cannot be
// read is counted and listed, not fatal.
func indexStore(ctx context.Context, env *Env, ix *index.Index, account, dir string, out *ReindexOut) error {
	st, err := store.Open(dir)
	if err != nil {
		return err
	}
	for entry := range st.All() {
		if err := ctx.Err(); err != nil {
			return err
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
	return nil
}

// installIndex puts a complete, closed index in place of the old one. The
// rename replaces the old file in one step, so a crash leaves one or the
// other; the old one's WAL files go first, as they belong to it.
func installIndex(built, path string) error {
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", filepath.Base(path+suffix), err)
		}
	}
	if err := os.Rename(built, path); err != nil {
		return fmt.Errorf("install new index: %w", err)
	}
	return atomicfile.SyncDir(filepath.Dir(path))
}

// openIndex opens the index. One that is missing or empty while there is an
// archive to index is rebuilt from the archive and the dumps first: started
// empty, it would soon be written over the dumps and lose every tag.
func openIndex(ctx context.Context, env *Env) (*index.Index, error) {
	ix, err := index.Open(ctx, env.Paths.IndexFile)
	if err != nil {
		return nil, err
	}
	n, err := ix.Count(ctx, query.Query{})
	if err != nil || n > 0 {
		return ix, err
	}
	if has, err := hasArchive(env); err != nil || !has {
		return ix, err
	}
	if err := ix.Close(); err != nil {
		return nil, err
	}
	// A name of its own: another nou may be rebuilding too.
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	built := env.Paths.IndexFile + ".rebuild-" + hex.EncodeToString(random)
	out, err := buildIndex(ctx, env, built)
	if err != nil {
		return nil, fmt.Errorf("the index was empty; rebuilding it from the archive: %w", err)
	}
	env.Log.Warn("the index was missing or empty: rebuilt it from the archive and the tag dumps",
		"messages", out.Messages, "failed", out.Failed)
	if err := installIndex(built, env.Paths.IndexFile); err != nil {
		return nil, err
	}
	return index.Open(ctx, env.Paths.IndexFile)
}

// hasArchive reports whether there is anything to build an index from: a
// dump, or a message in some account's archive.
func hasArchive(env *Env) (bool, error) {
	for _, dump := range []string{env.Paths.TagsDump, env.Paths.TransientDump} {
		if info, err := os.Stat(dump); err == nil && info.Size() > 0 {
			return true, nil
		}
	}
	accounts, err := archivedAccounts(env.Paths.ArchiveDir, env.Paths.TransientDir)
	if err != nil {
		return false, err
	}
	for _, account := range accounts {
		for _, dir := range []string{env.Paths.AccountArchive(account), env.Paths.TransientArchive(account)} {
			st, err := store.Open(dir)
			if err != nil {
				return false, err
			}
			for range st.All() {
				return true, nil
			}
		}
	}
	return false, nil
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
