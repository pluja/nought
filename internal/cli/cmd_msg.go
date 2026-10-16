// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/pluja/nought/internal/ops"
	"github.com/pluja/nought/internal/render"
)

const queryHelp = `
Query syntax: terms are ANDed; "a OR b" ORs two terms; a leading "-" negates
(put "--" before a query that starts with "-"). Fields: tag:, from:, to:,
subject:, account:, id: (Message-ID), hash: (message id shown by nou),
is:unread|flagged|inbox|replied, date:YYYY-MM-DD[..YYYY-MM-DD]. Bare words
and "quoted phrases" search the full text.`

func newSearchCmd(a *app) *cobra.Command {
	var in ops.SearchIn
	cmd := &cobra.Command{
		Use:     "search [query...]",
		Short:   ops.MsgSearch.Summary,
		Long:    ops.MsgSearch.Summary + ", newest first.\n" + queryHelp,
		Example: "  nou search tag:inbox is:unread\n  nou search from:bob date:2026-01-01.. -- -tag:done",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Query = queryFromArgs(args)
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.MsgSearch, in)
			return a.result(cmd, out, err, func(w io.Writer) error {
				for _, m := range out.Messages {
					if err := printf(w, "%s\n", messageLine(m)); err != nil {
						return err
					}
				}
				if out.More {
					return printf(w, "(more results: use --offset %d)\n", in.Offset+len(out.Messages))
				}
				return nil
			})
		},
	}
	cmd.Flags().IntVarP(&in.Limit, "limit", "l", ops.DefaultLimit, "maximum number of results")
	cmd.Flags().IntVar(&in.Offset, "offset", 0, "skip this many results")
	return cmd
}

// queryFromArgs joins the arguments into a query. An argument with spaces
// in it was quoted on the command line (subject:"verification code" reaches
// nou as one argument, subject:verification code): it is quoted again, so
// it stays one term instead of becoming two that match other mail.
func queryFromArgs(args []string) string {
	terms := make([]string, len(args))
	for i, a := range args {
		terms[i] = a
		if !strings.ContainsAny(a, " \t") || strings.Contains(a, `"`) {
			continue
		}
		prefix, value := "", a
		if field, rest, ok := strings.Cut(strings.TrimPrefix(a, "-"), ":"); ok && isFieldName(field) {
			prefix, value = a[:len(a)-len(rest)], rest
		} else if strings.HasPrefix(a, "-") {
			prefix, value = "-", a[1:]
		}
		terms[i] = prefix + `"` + value + `"`
	}
	return strings.Join(terms, " ")
}

func isFieldName(s string) bool {
	return s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyz") == ""
}

// messageLine formats one search result. Everything from the message is
// sanitized for the terminal.
func messageLine(m ops.MessageOut) string {
	date := "          "
	if m.Date != nil {
		date = m.Date.Local().Format("2006-01-02")
	}
	return m.ID + "  " + date + "  " + pad(m.Account, 10) + "  " + pad(sanitize(m.From), 28) + "  " +
		sanitize(m.Subject) + "  [" + sanitize(strings.Join(m.Tags, " ")) + "]"
}

// pad truncates or pads s to n runes.
func pad(s string, n int) string {
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n-1]) + "…"
	}
	return s + strings.Repeat(" ", n-utf8.RuneCountInString(s))
}

func newMsgCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "msg",
		Short: "Show, tag, archive, reply to and forward messages",
		Args:  noArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newMsgShowCmd(a), newMsgTagCmd(a), newMsgArchiveCmd(a), newMsgDeleteCmd(a), newMsgRestoreCmd(a),
		newMsgReplyCmd(a), newMsgForwardCmd(a), newMsgSaveCmd(a))
	return cmd
}

func newMsgShowCmd(a *app) *cobra.Command {
	var in ops.ShowIn
	var force bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: ops.MsgShow.Summary,
		Long: ops.MsgShow.Summary + ". HTML is converted to text and never rendered;\n" +
			"remote content is never loaded. Showing a message does not mark it read.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.ID = args[0]
			if in.Raw && !a.json && !force && isTerminal(a.opts.Stdout) {
				return fmt.Errorf("refusing to print a raw message to a terminal (it may contain escape sequences); pipe it, or use --force: %w", ops.ErrUsage)
			}
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.MsgShow, in)
			if err != nil {
				return err
			}
			if in.Raw && !a.json {
				_, err := io.WriteString(cmd.OutOrStdout(), out.Raw)
				return err
			}
			return a.emit(cmd, out, func(w io.Writer) error { return printMessage(w, out) })
		},
	}
	cmd.Flags().StringVarP(&in.Account, "account", "a", "", "account, if the id exists in several")
	cmd.Flags().BoolVar(&in.Raw, "raw", false, "print the raw message bytes (for piping to other programs)")
	cmd.Flags().BoolVar(&force, "force", false, "allow --raw output to a terminal")
	cmd.Flags().StringVar(&in.Prefer, "prefer", "", "version to show when there are both: html (as text) or plain")
	return cmd
}

func printMessage(w io.Writer, out ops.ShowOut) error {
	m := out.Message
	date := "unknown"
	if m.Date != nil {
		date = m.Date.Local().Format("Mon, 02 Jan 2006 15:04 MST")
	}
	rows := [][2]string{
		{"From", m.From}, {"To", strings.Join(m.To, ", ")}, {"Date", date},
		{"Subject", m.Subject}, {"Tags", strings.Join(m.Tags, " ")}, {"ID", m.ID + " (" + m.Account + ")"},
	}
	for _, r := range rows {
		if err := printf(w, "%-8s %s\n", r[0]+":", sanitize(r[1])); err != nil {
			return err
		}
	}
	if len(m.Server) == 0 {
		if err := printf(w, "Where:   local only (no longer on any server)\n"); err != nil {
			return err
		}
	}
	for _, loc := range m.Server {
		when := "stays (deletion is off)"
		if loc.LeavesAt != nil {
			when = "leaves the server on " + loc.LeavesAt.Local().Format("Mon, 02 Jan 2006")
		}
		if err := printf(w, "Where:   on server, %s (%s); %s\n", sanitize(loc.Folder), m.Account, when); err != nil {
			return err
		}
	}
	for i, att := range out.Attachments {
		if err := printf(w, "Attach:  %d %s (%s, %d bytes)\n", i+1, sanitize(att.Filename), sanitize(att.ContentType), att.Size); err != nil {
			return err
		}
	}
	if out.BodyFrom == "text/html" {
		if err := printf(w, "Note:    HTML converted to text; nothing was loaded.\n"); err != nil {
			return err
		}
	}
	body := out.Body
	if out.BodyFrom == "text/plain" {
		text, links := render.TextWithLinks(body, render.Styler{})
		body = render.WithLinkList(text, links, render.Styler{})
	}
	return printf(w, "\n%s\n", strings.TrimRight(render.Sanitize(body), "\n"))
}

func newMsgSaveCmd(a *app) *cobra.Command {
	in := ops.SaveIn{Dir: "."}
	cmd := &cobra.Command{
		Use:   "save <id> [number...]",
		Short: ops.MsgSave.Summary,
		Long: ops.MsgSave.Summary + ": all of them, or those numbered as in nou msg show.\n" +
			"Names are made safe, and an existing file is never overwritten (\"a (1).pdf\").",
		Example: "  nou msg save a3f9c2e1b0d4 --dir ~/Documents/invoices\n  nou msg save a3f9c2e1b0d4 2",
		Args:    rangeArgs(1, 100),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.ID = args[0]
			for _, s := range args[1:] {
				n, err := strconv.Atoi(s)
				if err != nil {
					return fmt.Errorf("%q is not an attachment number: %w", s, ops.ErrUsage)
				}
				in.Only = append(in.Only, n)
			}
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.MsgSave, in)
			return a.result(cmd, out, err, func(w io.Writer) error {
				for _, f := range out {
					if err := printf(w, "saved %s (%d bytes)\n", sanitize(f.Name), f.Size); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVarP(&in.Account, "account", "a", "", "account, if the id exists in several")
	cmd.Flags().StringVarP(&in.Dir, "dir", "d", ".", "directory to save into (created if missing)")
	return cmd
}

func newMsgTagCmd(a *app) *cobra.Command {
	var in ops.TagIn
	cmd := &cobra.Command{
		Use:   "tag <query...> --add TAG --remove TAG",
		Short: ops.MsgTag.Summary,
		Long: ops.MsgTag.Summary + ". A bare message id is accepted as the query.\n" +
			"Removing 'unread' marks messages read; other programs see it via Maildir flags.\n" + queryHelp,
		Example: "  nou msg tag a3f9c2e1b0d4 --add todo\n  nou msg tag from:newsletter@example.org --add newsletter --remove inbox",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Query = queryFromArgs(args)
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.MsgTag, in)
			return a.result(cmd, out, err, func(w io.Writer) error { return printTagOut(w, out) })
		},
	}
	cmd.Flags().StringVarP(&in.Account, "account", "a", "", "account, if a bare message id exists in several")
	cmd.Flags().StringSliceVar(&in.Add, "add", nil, "tags to add (repeatable or comma-separated)")
	cmd.Flags().StringSliceVar(&in.Remove, "remove", nil, "tags to remove (repeatable or comma-separated)")
	cmd.Flags().BoolVarP(&in.DryRun, "dry-run", "n", false, "report what would change without changing anything")
	return cmd
}

func newMsgArchiveCmd(a *app) *cobra.Command {
	var in ops.ArchiveIn
	cmd := &cobra.Command{
		Use:     "archive <query...>",
		Short:   ops.MsgArchive.Summary,
		Long:    ops.MsgArchive.Summary + ". A bare message id is accepted as the query.\n" + queryHelp,
		Example: "  nou msg archive a3f9c2e1b0d4\n  nou msg archive tag:inbox from:notifications@example.org",
		Args:    cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Query = queryFromArgs(args)
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.MsgArchive, in)
			return a.result(cmd, out, err, func(w io.Writer) error { return printTagOut(w, out) })
		},
	}
	cmd.Flags().StringVarP(&in.Account, "account", "a", "", "account, if a bare message id exists in several")
	cmd.Flags().BoolVarP(&in.DryRun, "dry-run", "n", false, "report what would change without changing anything")
	return cmd
}

func printTagOut(w io.Writer, out ops.TagOut) error {
	verb := "changed"
	if out.DryRun {
		verb = "would change"
	}
	return printf(w, "matched %d, %s %d\n", out.Matched, verb, out.Changed)
}

// isTerminal reports whether v (a reader or writer) is an interactive terminal.
func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
