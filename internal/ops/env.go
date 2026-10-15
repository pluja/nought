// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"log/slog"
	"time"

	"nought/internal/config"
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
	Paths config.Paths
	Log   *slog.Logger
	Now   func() time.Time
	Build BuildInfo

	config *config.Config
}

// NewEnv returns an Env. Nothing is opened until an op needs it.
func NewEnv(paths config.Paths, log *slog.Logger, now func() time.Time, build BuildInfo) *Env {
	return &Env{Paths: paths, Log: log, Now: now, Build: build}
}

// Config loads the configuration on first use.
func (e *Env) Config() (*config.Config, error) {
	if e.config == nil {
		c, err := config.Load(e.Paths.ConfigFile)
		if err != nil {
			return nil, err
		}
		e.config = c
	}
	return e.config, nil
}

// Close releases everything the Env opened.
func (e *Env) Close() error {
	return nil
}
