// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
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
		e.Error.Code, e.Error.Message = code, err.Error()
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(e)
		return
	}
	_, _ = fmt.Fprintf(w, "nou: %v\n", err)
}
