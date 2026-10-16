// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/pluja/nought/internal/ops"
	"github.com/pluja/nought/internal/tui"
)

func newTUICmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the terminal interface (also: 'nou' with no arguments)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !a.interactive() {
				return fmt.Errorf("the TUI needs a terminal: %w", ops.ErrUsage)
			}
			return a.runTUI(cmd)
		},
	}
}

func (a *app) runTUI(cmd *cobra.Command) error {
	return tui.Run(cmd.Context(), a.env, tui.Options{Input: a.opts.Stdin, Output: a.opts.Stdout,
		Editor: a.editorCommand()})
}
