// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pluja/nought/internal/compose"
	"github.com/pluja/nought/internal/ops"
	"github.com/pluja/nought/internal/render"
)

func newDraftCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "draft",
		Short: "List, edit, send and discard drafts",
		Args:  noArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newDraftListCmd(a), newDraftShowCmd(a), newDraftEditCmd(a), newDraftSendCmd(a), newDraftDiscardCmd(a))
	return cmd
}

func newDraftListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: ops.DraftList.Summary,
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.DraftList, struct{}{})
			return a.result(cmd, out, err, func(w io.Writer) error {
				for _, d := range out {
					line := d.ID + "  " + d.Updated.Local().Format("2006-01-02 15:04") + "  " + pad(d.Kind, 9) + "  " +
						pad(sanitize(strings.Join(d.To, ", ")), 28) + "  " + sanitize(d.Subject)
					if d.LastError != "" {
						line += "  (not sent: " + sanitize(d.LastError) + ")"
					}
					if err := printf(w, "%s\n", line); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}

func newDraftShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: ops.DraftShow.Summary + ", as in the editor, with its checks",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.DraftShow, ops.DraftIDIn{ID: args[0]})
			return a.result(cmd, out, err, func(w io.Writer) error {
				// Replies quote mail: sanitize it for the terminal.
				if err := printf(w, "%s\n", strings.TrimRight(render.Sanitize(compose.Render(out.Draft)), "\n")); err != nil {
					return err
				}
				return printProblems(w, out.Problems)
			})
		},
	}
}

func newDraftEditCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "edit <id>",
		Short: "Edit a draft in your editor, then send or keep it",
		Long: "Edit a draft in your editor, then send or keep it. Needs a terminal; scripts can\n" +
			"edit the file named by nou draft show --json (\"path\") and run nou draft send.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !a.interactive() || a.json {
				return fmt.Errorf("nou draft edit needs a terminal: %w", ops.ErrUsage)
			}
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.DraftShow, ops.DraftIDIn{ID: args[0]})
			if err != nil {
				return err
			}
			return a.editLoop(cmd, out)
		},
	}
}

func newDraftSendCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "send <id>",
		Short: "Send a draft now",
		Args:  exactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return a.sendNow(cmd, args[0]) },
	}
}

func newDraftDiscardCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "discard <id>",
		Short: ops.DraftDiscard.Summary,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.DraftDiscard, ops.DraftIDIn{ID: args[0]})
			return a.result(cmd, out, err, func(w io.Writer) error { return printf(w, "discarded %s\n", args[0]) })
		},
	}
}

func newOutboxCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "outbox",
		Short: "Messages waiting to be sent",
		Long: "Messages waiting to be sent. A message that could not be sent stays here:\n" +
			"temporary failures are retried at each sync, refused ones wait for you.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newOutboxListCmd(a), newOutboxSendCmd(a), newOutboxRetryCmd(a), newOutboxCancelCmd(a))
	return cmd
}

func newOutboxListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: ops.OutboxList.Summary,
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.OutboxList, struct{}{})
			return a.result(cmd, out, err, func(w io.Writer) error {
				for _, e := range out {
					line := e.ID + "  " + pad(e.State, 8) + "  " + sanitize(e.Subject)
					if e.State == ops.StateWaiting {
						line += "  (from " + e.SendAfter.Local().Format("15:04:05") + ")"
					}
					if e.LastError != "" {
						line += "  " + sanitize(e.LastError)
					}
					if err := printf(w, "%s\n", line); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}

func newOutboxSendCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "send",
		Short: ops.OutboxFlush.Summary,
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.OutboxFlush, ops.FlushIn{})
			if err == nil && out.Failed > 0 {
				err = fmt.Errorf("the server refused %d message(s); see nou outbox list: %w", out.Failed, ops.ErrPartial)
			}
			a.notes(cmd, out.Notes)
			return a.result(cmd, out, err, func(w io.Writer) error {
				return printf(w, "sent %d, failed %d, waiting %d\n", out.Sent, out.Failed, out.Waiting)
			})
		},
	}
}

func newOutboxRetryCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "retry <id>",
		Short: "Try sending a held message again, now",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.OutboxRetry, ops.DraftIDIn{ID: args[0]})
			if err != nil {
				return err
			}
			return a.flushAndReport(cmd, e)
		},
	}
}

func newOutboxCancelCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <id>",
		Short: ops.OutboxCancel.Summary,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.OutboxCancel, ops.DraftIDIn{ID: args[0]})
			return a.result(cmd, out, err, func(w io.Writer) error { return printDraftLine(w, out) })
		},
	}
}
