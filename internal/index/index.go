// SPDX-License-Identifier: AGPL-3.0-or-later

// Package index is the SQLite index over the archive: message metadata,
// tags and full-text search. It can always be rebuilt from the Maildir files
// plus the tag dump, so it is never the only copy of anything.
package index

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// Index is an open index database.
type Index struct {
	db *sql.DB
}

// pragmas are fixed: they are applied by the driver to every connection.
const pragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"

// Open opens (creating if needed) the index at path and applies migrations.
func Open(ctx context.Context, path string) (*Index, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("index path must not contain '?' or '#': %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create index directory: %w", err)
	}
	// Create the file with private permissions before SQLite does.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create index: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("create index: %w", err)
	}

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+pragmas)
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	// One connection: SQLite has a single writer anyway, and it means
	// in-process callers never see SQLITE_BUSY. Other processes wait up to
	// busy_timeout.
	db.SetMaxOpenConns(1)
	ix := &Index{db: db}
	if err := ix.migrate(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return ix, nil
}

// Close closes the database.
func (ix *Index) Close() error { return ix.db.Close() }

// migrate applies every embedded migration newer than PRAGMA user_version,
// each in its own transaction. Migration files are named NNNN_name.sql and
// are never edited once released.
func (ix *Index) migrate(ctx context.Context) error {
	var version int
	if err := ix.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	if version > len(names) {
		return fmt.Errorf("index schema version %d is newer than this nou supports (%d); upgrade nou", version, len(names))
	}
	for i, name := range names[version:] {
		script, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		next := version + i + 1
		err = ix.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(script)); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", next))
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %s: %w", filepath.Base(name), err)
		}
	}
	return nil
}

// tx runs fn in a transaction, committing on success.
func (ix *Index) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
