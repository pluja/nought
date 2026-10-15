// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"errors"

	"nought/internal/ops"
)

// Exit codes. They are part of the CLI's stable interface; see the spec.
const (
	exitOK         = 0
	exitInternal   = 1
	exitUsage      = 2
	exitPartial    = 3
	exitPermission = 4
	exitNotFound   = 5
	exitBusy       = 6
	// exitInterrupted follows the shell convention 128 + SIGINT.
	exitInterrupted = 130
)

// exitCode is the single place where errors become exit codes and stable
// error code strings.
func exitCode(err error) (code int, name string) {
	switch {
	case errors.Is(err, context.Canceled):
		return exitInterrupted, "interrupted"
	case errors.Is(err, ops.ErrUsage):
		return exitUsage, "usage"
	case errors.Is(err, ops.ErrPartial):
		return exitPartial, "partial"
	case errors.Is(err, ops.ErrPermission):
		return exitPermission, "permission"
	case errors.Is(err, ops.ErrAmbiguous):
		return exitNotFound, "ambiguous"
	case errors.Is(err, ops.ErrNotFound):
		return exitNotFound, "not_found"
	case errors.Is(err, ops.ErrBusy):
		return exitBusy, "busy"
	default:
		return exitInternal, "internal"
	}
}
