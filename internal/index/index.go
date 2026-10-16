// SPDX-License-Identifier: AGPL-3.0-or-later

// Package index is the SQLite index over the archive: message metadata,
// tags and full-text search. It can always be rebuilt from the Maildir files
// plus the tag dump, so it is never the only copy of anything.
package index

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"modernc.org/sqlite" // also registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// Index is an open index database.
type Index struct {
	db *sql.DB
}

// pragmas are fixed: they are applied by the driver to every connection.
// busy_timeout comes first so it already applies while switching to WAL.
const pragmas = "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)" +
	"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate"

// registerFold gives SQLite fold(), lower() for every script: SQLite's
// own folds ASCII only, so subject:ärger missed "ÄRGER". SQLite's function
// registry belongs to the process, hence one registration.
var registerFold = sync.OnceValue(func() error {
	return sqlite.RegisterDeterministicScalarFunction("fold", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		switch v := args[0].(type) {
		case string:
			return strings.ToLower(v), nil
		case []byte:
			return strings.ToLower(string(v)), nil
		}
		return args[0], nil
	})
})

// Open opens (creating if needed) the index at path and applies migrations.
func Open(ctx context.Context, path string) (*Index, error) {
	if err := registerFold(); err != nil {
		return nil, err
	}
	// The path becomes a file: URI, where these characters are special.
	if strings.ContainsAny(path, "?#%") {
		return nil, fmt.Errorf("index path must not contain '?', '#' or '%%': %s", path)
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
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for {
		// Read the version inside the (immediate, write-locked) transaction
		// so concurrent first opens cannot both apply the same migration.
		done := false
		err := retryBusy(ctx, isBusy, func() error {
			return ix.tx(ctx, func(tx *sql.Tx) error {
				var version int
				if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
					return fmt.Errorf("read schema version: %w", err)
				}
				if version > len(names) {
					return fmt.Errorf("index schema version %d is newer than this nou supports (%d); upgrade nou", version, len(names))
				}
				if version == len(names) {
					done = true
					return nil
				}
				script, err := migrations.ReadFile(names[version])
				if err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, string(script)); err != nil {
					return fmt.Errorf("apply migration %s: %w", filepath.Base(names[version]), err)
				}
				_, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version+1))
				return err
			})
		})
		if err != nil || done {
			return err
		}
	}
}

// retryBusy runs fn until it is not busy: concurrent first opens can see
// SQLITE_BUSY while another process switches the new database to WAL, which
// the busy handler does not cover. It waits a little longer each time, up to
// 50 retries (about 14 s), and stops when ctx ends.
func retryBusy(ctx context.Context, busy func(error) bool, fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if !busy(err) || attempt == 50 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(20+attempt*10) * time.Millisecond):
		}
	}
}

// sqliteBusy is SQLITE_BUSY's primary result code.
const sqliteBusy = 5

func isBusy(err error) bool {
	e, ok := errors.AsType[*sqlite.Error](err)
	return ok && e.Code()&0xff == sqliteBusy
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
