// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pluja/nought/internal/ops"
)

func newMsgDeleteCmd(a *app) *cobra.Command {
	var in ops.DeleteIn
	cmd := &cobra.Command{
		Use:   "delete <query...>",
		Short: ops.MsgDelete.Summary,
		Long: "Move messages to the trash: hidden everywhere, kept out of backups (transient\n" +
			"folder), deleted for good after trash_days (default 30). With --now they are\n" +
			"deleted for good at once. Copies still on a server are deleted there at the\n" +
			"next sync, even when the account's automatic deletion is off.\n" + queryHelp,
		Example: "  nou msg delete a3f9c2e1b0d4\n  nou msg delete --now subject:\"verification code\"",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Query = strings.Join(args, " ")
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.MsgDelete, in)
			return a.result(cmd, out, err, func(w io.Writer) error {
				switch {
				case out.DryRun:
					return printf(w, "would delete %d\n", out.Matched)
				case in.Now:
					return printf(w, "deleted %d for good; %d wait for the next sync to leave the server first\n", out.Purged, out.Pending)
				default:
					return printf(w, "moved %d to the trash (undo: nou msg restore tag:deleted)\n", out.Matched)
				}
			})
		},
	}
	cmd.Flags().BoolVar(&in.Now, "now", false, "delete for good at once instead of moving to the trash")
	cmd.Flags().StringVarP(&in.Account, "account", "a", "", "account, if a bare message id exists in several")
	cmd.Flags().BoolVarP(&in.DryRun, "dry-run", "n", false, "report what would be deleted")
	return cmd
}

func newMsgRestoreCmd(a *app) *cobra.Command {
	var in ops.RestoreIn
	cmd := &cobra.Command{
		Use:   "restore <query...>",
		Short: ops.MsgRestore.Summary,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Query = strings.Join(args, " ")
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.MsgRestore, in)
			return a.result(cmd, out, err, func(w io.Writer) error {
				return printf(w, "restored %d\n", out.Changed)
			})
		},
	}
	cmd.Flags().StringVarP(&in.Account, "account", "a", "", "account, if a bare message id exists in several")
	return cmd
}

func newTrashCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trash",
		Short: "Empty the trash (list it with: nou search tag:deleted)",
		Args:  noArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "empty",
		Short: ops.TrashEmpty.Summary,
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.TrashEmpty, struct{}{})
			return a.result(cmd, out, err, func(w io.Writer) error {
				return printf(w, "deleted %d for good; %d wait for the next sync to leave the server first\n", out.Purged, out.Pending)
			})
		},
	})
	return cmd
}
