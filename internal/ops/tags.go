// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/pluja/nought/internal/atomicfile"
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
