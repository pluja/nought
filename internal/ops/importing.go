// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"

	"nought/internal/importer"
	"nought/internal/index"
	"nought/internal/mailmsg"
	"nought/internal/store"
)

// ImportIn selects what to import.
type ImportIn struct {
	Path    string   `json:"path"`
	Account string   `json:"account"`
	Format  string   `json:"format,omitempty"` // "maildir" or "mbox"; detected if empty
	Inbox   bool     `json:"inbox,omitempty"`  // also tag imported messages "inbox"
	Tags    []string `json:"tags,omitempty"`   // extra tags for every imported message
	DryRun  bool     `json:"dry_run,omitempty"`
}

// Failure describes one item that could not be processed.
type Failure struct {
	Source string `json:"source"`
	Error  string `json:"error"`
}

// ImportOut summarizes an import.
type ImportOut struct {
	Imported   int       `json:"imported"`
	Duplicates int       `json:"duplicates"`
	Deleted    int       `json:"deleted"` // marked deleted by the source program; skipped
	Failed     int       `json:"failed"`
	Failures   []Failure `json:"failures,omitempty"` // the first maxFailures
	DryRun     bool      `json:"dry_run"`
}

const maxFailures = 50

func (o *ImportOut) fail(source string, err error) {
	o.Failed++
	if len(o.Failures) < maxFailures {
		o.Failures = append(o.Failures, Failure{Source: source, Error: err.Error()})
	}
}

// ImportRun imports messages from a Maildir tree, .eml files or an mbox
// file. Messages already in the account (same content or same Message-ID)
// are skipped.
var ImportRun = Op[ImportIn, ImportOut]{
	Name:    "import.run",
	Summary: "Import messages from a Maildir tree, .eml files or an mbox file",
	Perm:    Tag,
	Run:     runImport,
}

func runImport(ctx context.Context, env *Env, in ImportIn) (ImportOut, error) {
	out := ImportOut{DryRun: in.DryRun}
	if in.Path == "" || in.Account == "" {
		return out, usageError("a path and an account are required")
	}
	for _, t := range in.Tags {
		if err := index.ValidateTag(t); err != nil {
			return out, usageError("%v", err)
		}
	}
	items, closeSource, err := openSource(in.Path, in.Format)
	if err != nil {
		return out, err
	}
	defer func() { _ = closeSource() }()
	ix, err := env.Index(ctx)
	if err != nil {
		return out, err
	}
	if _, err := env.Store(in.Account); err != nil {
		return out, err
	}

	for item, err := range items {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if err != nil {
			out.fail(item.Source, err)
			continue
		}
		if item.Deleted {
			out.Deleted++
			continue
		}
		dup, err := isDuplicate(ctx, ix, in.Account, item.Raw)
		if err != nil {
			return out, err
		}
		if dup {
			out.Duplicates++
			continue
		}
		if in.DryRun {
			out.Imported++
			continue
		}
		tags := mergeTags([]string{folderTag(item.Folder)}, in.Tags)
		if in.Inbox {
			tags = mergeTags(tags, []string{TagInbox})
		}
		if _, _, err := archive(ctx, env, in.Account, item.Raw, item.Flags, tags); err != nil {
			out.fail(item.Source, err)
			continue
		}
		out.Imported++
	}

	if !in.DryRun && out.Imported > 0 {
		if err := writeTagsDump(ctx, env); err != nil {
			return out, err
		}
	}
	if out.Failed > 0 {
		return out, fmt.Errorf("%d of %d messages failed: %w", out.Failed, out.Failed+out.Imported, ErrPartial)
	}
	return out, nil
}

func isDuplicate(ctx context.Context, ix *index.Index, account string, raw []byte) (bool, error) {
	if dup, err := ix.HasHash(ctx, account, store.Hash(raw)); err != nil || dup {
		return dup, err
	}
	h, _ := mailmsg.Parse(raw)
	return ix.HasMessageID(ctx, account, h.MessageID)
}

func openSource(path, format string) (iter.Seq2[importer.Item, error], func() error, error) {
	if format == "" {
		var err error
		if format, err = importer.Detect(path); err != nil {
			return nil, nil, usageError("%v", err)
		}
	}
	switch format {
	case importer.FormatMaildir:
		return importer.Maildir(path), func() error { return nil }, nil
	case importer.FormatMbox:
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil, fmt.Errorf("%s: %w", path, ErrNotFound)
			}
			return nil, nil, err
		}
		folder := filepath.Base(path)
		return importer.Mbox(f, folder, path), f.Close, nil
	default:
		return nil, nil, usageError("unknown format %q (use maildir or mbox)", format)
	}
}
