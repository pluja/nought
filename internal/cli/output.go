// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"nought/internal/ops"
	"nought/internal/render"
)

// emit prints an op's output: JSON with --json, otherwise the human form.
func (a *app) emit(cmd *cobra.Command, out any, human func(w io.Writer) error) error {
	w := cmd.OutOrStdout()
	if a.json {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}
	return human(w)
}

// result emits an op's output and returns its error. Output is still shown
// for partial failures, which carry useful per-item results.
func (a *app) result(cmd *cobra.Command, out any, err error, human func(w io.Writer) error) error {
	if err != nil && !errors.Is(err, ops.ErrPartial) {
		return err
	}
	if emitErr := a.emit(cmd, out, human); emitErr != nil {
		return emitErr
	}
	return err
}

// sanitize makes mail-derived text safe for the terminal.
func sanitize(s string) string { return render.SanitizeLine(s) }

// printf writes formatted text, for human output functions.
func printf(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format, args...)
	return err
}

type jsonError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeError reports a failed command on stderr.
func writeError(w io.Writer, asJSON bool, code string, err error) {
	if asJSON {
		var e jsonError
		e.Error.Code, e.Error.Message = code, sanitize(err.Error())
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(e)
		return
	}
	// Errors can quote file names, queries or dump lines: sanitize them.
	_, _ = fmt.Fprintf(w, "nou: %s\n", sanitize(err.Error()))
}
