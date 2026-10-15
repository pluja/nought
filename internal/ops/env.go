// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"sync"
	"time"

	"nought/internal/autoconfig"
	"nought/internal/config"
	"nought/internal/index"
	"nought/internal/secret"
	"nought/internal/store"
)

// BuildInfo describes the running binary.
type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	GoVersion string `json:"go_version"`
}

// Env carries the dependencies every op may use. Expensive resources are
// opened lazily so cheap commands stay cheap; Close releases them.
type Env struct {
	Paths   config.Paths
	Log     *slog.Logger
	Now     func() time.Time
	Build   BuildInfo
	Secrets secret.Store
	// TLS is the base TLS configuration for mail servers; nil means the
	// system's trusted roots. Tests use it to trust a test certificate.
	TLS *tls.Config
	// IMAPTimeout bounds each read and write to a mail server; 0 means the
	// default. Tests shorten it.
	IMAPTimeout time.Duration
	// Network is used by account discovery; tests replace it.
	Network autoconfig.Network

	mu     sync.Mutex
	config *config.Config
	index  *index.Index
	stores map[string]*store.Maildir
}

// NewEnv returns an Env. Nothing is opened until an op needs it.
func NewEnv(paths config.Paths, log *slog.Logger, now func() time.Time, build BuildInfo, secrets secret.Store) *Env {
	return &Env{Paths: paths, Log: log, Now: now, Build: build, Secrets: secrets, stores: make(map[string]*store.Maildir)}
}

// Config loads the configuration on first use.
func (e *Env) Config() (*config.Config, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.config == nil {
		c, err := config.Load(e.Paths.ConfigFile)
		if err != nil {
			return nil, err
		}
		e.config = c
	}
	return e.config, nil
}

// Index opens the index on first use.
func (e *Env) Index(ctx context.Context) (*index.Index, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.index == nil {
		ix, err := index.Open(ctx, e.Paths.IndexFile)
		if err != nil {
			return nil, err
		}
		e.index = ix
	}
	return e.index, nil
}

// closeIndex closes the index so it can be replaced (see reindex).
func (e *Env) closeIndex() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.index == nil {
		return nil
	}
	err := e.index.Close()
	e.index = nil
	return err
}

// Store opens an account's Maildir on first use. A Maildir is not safe for
// concurrent use; callers use one account per goroutine.
func (e *Env) Store(account string) (*store.Maildir, error) {
	if err := config.ValidateAccountName(account); err != nil {
		return nil, usageError("account %q: %v", account, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if st, ok := e.stores[account]; ok {
		return st, nil
	}
	st, err := store.Open(e.Paths.AccountArchive(account))
	if err != nil {
		return nil, err
	}
	e.stores[account] = st
	return st, nil
}

// Close releases everything the Env opened.
func (e *Env) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var errs []error
	for name, st := range e.stores {
		errs = append(errs, st.Close())
		delete(e.stores, name)
	}
	if e.index != nil {
		errs = append(errs, e.index.Close())
		e.index = nil
	}
	return errors.Join(errs...)
}
