// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"nought/internal/ops"
)

func newSyncCmd(a *app) *cobra.Command {
	var in ops.SyncIn
	cmd := &cobra.Command{
		Use:   "sync [account...]",
		Short: ops.SyncRun.Summary,
		Long: ops.SyncRun.Summary + ".\n\n" +
			"New messages are archived (verified on disk) and indexed. For accounts with\n" +
			"deletion enabled, messages are deleted from the server once their folder's\n" +
			"keep_days have passed, only if the local copy still verifies, and only with\n" +
			"UID EXPUNGE. Every deletion is journaled first. Re-running is always safe.",
		Example: "  nou sync --dry-run\n  nou sync personal",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Accounts = args
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.SyncRun, in)
			return a.result(cmd, out, err, func(w io.Writer) error { return printSync(w, out) })
		},
	}
	cmd.Flags().BoolVarP(&in.DryRun, "dry-run", "n", false, "connect and report, without archiving or deleting")
	return cmd
}

func printSync(w io.Writer, out ops.SyncOut) error {
	for _, acct := range out.Accounts {
		deletion := "deletion disabled"
		if acct.Deletion {
			deletion = "deletion enabled"
		}
		if err := printf(w, "%s (%s)\n", acct.Account, deletion); err != nil {
			return err
		}
		if acct.Error != "" {
			if err := printf(w, "  error: %s\n", sanitize(acct.Error)); err != nil {
				return err
			}
		}
		for _, f := range acct.Folders {
			if err := printf(w, "  %-24s %s\n", sanitize(f.Folder), folderSummary(f, out.DryRun)); err != nil {
				return err
			}
			for _, n := range f.Notes {
				if err := printf(w, "  %24s note: %s\n", "", sanitize(n)); err != nil {
					return err
				}
			}
			if f.Error != "" {
				if err := printf(w, "  %24s error: %s\n", "", sanitize(f.Error)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func folderSummary(f ops.FolderSyncOut, dryRun bool) string {
	if f.Action == "ignore" {
		return "ignored"
	}
	var parts []string
	if dryRun {
		parts = append(parts, fmt.Sprintf("%d new", f.New), fmt.Sprintf("%d due for deletion", f.Due))
	} else {
		parts = append(parts, fmt.Sprintf("archived %d", f.Fetched), fmt.Sprintf("deleted %d", f.Expunged))
		if waiting := f.Due - f.Expunged; waiting > 0 {
			parts = append(parts, fmt.Sprintf("%d due but kept", waiting))
		}
	}
	return strings.Join(parts, ", ") + fmt.Sprintf(" (keep %dd)", f.KeepDays)
}

func newAccountTestCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "test <name>",
		Short: ops.AccountTest.Summary,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.AccountTest, ops.AccountName{Name: args[0]})
			return a.result(cmd, out, err, func(w io.Writer) error {
				uidplus := "yes"
				if !out.UIDPlus {
					uidplus = "no (nou will archive but never delete from this server)"
				}
				if err := printf(w, "connected and logged in to %s\nUIDPLUS: %s\nfolders:\n", out.Account, uidplus); err != nil {
					return err
				}
				for _, f := range out.Folders {
					what := "ignore"
					if f.Policy.Action == "archive" {
						what = fmt.Sprintf("archive, keep %d days on server", f.Policy.KeepDays)
					}
					if err := printf(w, "  %-24s %s\n", sanitize(f.Folder), what); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}
