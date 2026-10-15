// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"nought/internal/ops"
)

func newRootCmd(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:   "nou",
		Short: "nought: a privacy-first Inbox Zero mail client",
		Long: "nought archives your mail locally (Maildir + SQLite) and keeps the server empty.\n" +
			"Every command supports --json for scripts and agents.",
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: a.setup,
		Annotations:       map[string]string{noPaths: "true"},
		Args:              cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("unknown command %q (see 'nou --help'): %w", args[0], ops.ErrUsage)
			}
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fmt.Errorf("%w: %w", err, ops.ErrUsage)
	})

	flags := root.PersistentFlags()
	flags.BoolVar(&a.json, "json", false, "print machine-readable JSON")
	flags.BoolVarP(&a.verbose, "verbose", "v", false, "log debug information to stderr")

	root.AddCommand(
		newSearchCmd(a),
		newMsgCmd(a),
		newAccountCmd(a),
		newImportCmd(a),
		newReindexCmd(a),
		newTagsCmd(a),
		newPathsCmd(a),
		newVersionCmd(a),
	)
	return root
}

// exactArgs is cobra.ExactArgs with errors classified as usage errors.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(n)(cmd, args); err != nil {
			return fmt.Errorf("%w: %w", err, ops.ErrUsage)
		}
		return nil
	}
}

// rangeArgs is cobra.RangeArgs with errors classified as usage errors.
func rangeArgs(min, max int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.RangeArgs(min, max)(cmd, args); err != nil {
			return fmt.Errorf("%w: %w", err, ops.ErrUsage)
		}
		return nil
	}
}

// noArgs is cobra.NoArgs with errors classified as usage errors.
func noArgs(cmd *cobra.Command, args []string) error {
	return exactArgs(0)(cmd, args)
}
