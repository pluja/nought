// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"

	"nought/internal/config"
)

// PathsOut lists data locations and what to back up.
type PathsOut struct {
	config.Paths
	Backup struct {
		Include  []string `json:"include"`
		Optional []string `json:"optional"`
		Never    []string `json:"never"` // deleted and expiring mail
	} `json:"backup"`
}

// PathsShow reports where nought keeps its data.
var PathsShow = Op[struct{}, PathsOut]{
	Name:    "paths.show",
	Summary: "Show data locations and the recommended backup set",
	Perm:    Read,
	Run: func(_ context.Context, env *Env, _ struct{}) (PathsOut, error) {
		out := PathsOut{Paths: env.Paths}
		out.Backup.Include, out.Backup.Optional, out.Backup.Never = env.Paths.BackupSet()
		return out, nil
	},
}

// VersionShow reports build information.
var VersionShow = Op[struct{}, BuildInfo]{
	Name:    "version.show",
	Summary: "Show version information",
	Perm:    Read,
	Run: func(_ context.Context, env *Env, _ struct{}) (BuildInfo, error) {
		return env.Build, nil
	},
}
