// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/pluja/nought/internal/index"
	"github.com/pluja/nought/internal/ops"
)

func newImportCmd(a *app) *cobra.Command {
	var in ops.ImportIn
	cmd := &cobra.Command{
		Use:   "import <path>",
		Short: ops.ImportRun.Summary,
		Long: ops.ImportRun.Summary + ".\n\n" +
			"A directory is read as a Maildir tree (cur/ and new/ folders, e.g. a Thunderbird\n" +
			"export) plus any *.eml files; a file is read as mbox. Messages already in the\n" +
			"account, by content or Message-ID, are skipped. Each message is tagged with its\n" +
			"source folder (folder:<name>). Imported mail is not put in the inbox unless --inbox.",
		Example: "  nou import ~/thunderbird-export --account personal --dry-run",
		Args:    exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Path = args[0]
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.ImportRun, in)
			return a.result(cmd, out, err, func(w io.Writer) error {
				verb := "imported"
				if out.DryRun {
					verb = "would import"
				}
				if err := printf(w, "%s %d, duplicates %d, deleted in source %d, failed %d\n",
					verb, out.Imported, out.Duplicates, out.Deleted, out.Failed); err != nil {
					return err
				}
				return printFailures(w, out.Failures)
			})
		},
	}
	f := cmd.Flags()
	f.StringVarP(&in.Account, "account", "a", "", "account to import into (required)")
	f.StringVar(&in.Format, "format", "", "maildir or mbox (default: detect)")
	f.BoolVar(&in.Inbox, "inbox", false, "tag imported messages inbox")
	f.StringSliceVarP(&in.Tags, "tag", "t", nil, "extra tag for every imported message (repeatable)")
	f.BoolVarP(&in.DryRun, "dry-run", "n", false, "report what would be imported without changing anything")
	return cmd
}

func printFailures(w io.Writer, failures []ops.Failure) error {
	for _, f := range failures {
		if err := printf(w, "  failed: %s: %s\n", sanitize(f.Source), sanitize(f.Error)); err != nil {
			return err
		}
	}
	return nil
}

func newReindexCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "reindex",
		Short: ops.IndexRebuild.Summary,
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.IndexRebuild, struct{}{})
			return a.result(cmd, out, err, func(w io.Writer) error {
				if err := printf(w, "indexed %d messages in %d accounts; restored tags for %d (%d not found, %d tags skipped)\n",
					out.Messages, out.Accounts, out.Restored.Applied, out.Restored.Missing, out.Restored.SkippedTags); err != nil {
					return err
				}
				return printFailures(w, out.Failures)
			})
		},
	}
}

func newTagsCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tags",
		Short: "List, dump and restore tags",
		Args:  noArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: ops.TagsList.Summary,
			Args:  noArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.TagsList, struct{}{})
				return a.result(cmd, out, err, func(w io.Writer) error {
					for _, tc := range out {
						if err := printf(w, "%7d  %s\n", tc.Count, sanitize(tc.Tag)); err != nil {
							return err
						}
					}
					return nil
				})
			},
		},
		newTagsDumpCmd(a),
		&cobra.Command{
			Use:   "restore <file>",
			Short: ops.TagsRestore.Summary,
			Args:  exactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.TagsRestore, ops.TagsRestoreIn{Path: args[0]})
				return a.result(cmd, out, err, func(w io.Writer) error {
					return printRestore(w, out)
				})
			},
		},
	)
	return cmd
}

func printRestore(w io.Writer, r index.RestoreResult) error {
	return printf(w, "restored tags for %d messages (%d not found, %d tags skipped)\n", r.Applied, r.Missing, r.SkippedTags)
}

func newTagsDumpCmd(a *app) *cobra.Command {
	var in ops.TagsDumpIn
	cmd := &cobra.Command{
		Use:   "dump [file]",
		Short: ops.TagsDump.Summary,
		Long: ops.TagsDump.Summary + ". Without a file, the dump is printed. Deleted and expiring\n" +
			"mail is left out unless --trash is given.",
		Args: rangeArgs(0, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				in.Path = args[0]
			}
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.TagsDump, in)
			return a.result(cmd, out, err, func(w io.Writer) error {
				if out.Path != "" {
					return printf(w, "wrote %s\n", out.Path)
				}
				return printf(w, "%s", out.Dump)
			})
		},
	}
	cmd.Flags().BoolVar(&in.Trash, "trash", false, "include deleted and expiring mail")
	return cmd
}
