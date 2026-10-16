// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pluja/nought/internal/compose"
	"github.com/pluja/nought/internal/ops"
)

const composeHelp = `
In a terminal, nou opens your editor ($VISUAL, then $EDITOR) on the message,
then asks whether to send it. Scripts and agents give the body with
--body-file and send at once with --send; without --send the message is kept
as a draft (see nou draft).`

// composeFlags are shared by the commands that start a message.
type composeFlags struct {
	bodyFile string
	send     bool
}

func (f *composeFlags) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.bodyFile, "body-file", "", "read the text from a file ('-' for stdin)")
	cmd.Flags().BoolVar(&f.send, "send", false, "send at once, without the editor")
}

func (f *composeFlags) body(cmd *cobra.Command) (string, error) {
	var b []byte
	var err error
	switch f.bodyFile {
	case "":
		return "", nil
	case "-":
		b, err = io.ReadAll(cmd.InOrStdin())
	default:
		b, err = os.ReadFile(f.bodyFile)
	}
	if err != nil {
		return "", fmt.Errorf("read the body: %w", err)
	}
	return string(b), nil
}

func newComposeCmd(a *app) *cobra.Command {
	var in ops.DraftNewIn
	var f composeFlags
	cmd := &cobra.Command{
		Use:   "compose",
		Short: "Write a new message",
		Long:  "Write a new message." + composeHelp,
		Example: "  nou compose --to alice@example.org\n" +
			"  echo 'See you at 10.' | nou compose --to bob@example.org --subject Tomorrow --body-file - --send",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var err error
			if in.Body, err = f.body(cmd); err != nil {
				return err
			}
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.DraftNew, in)
			if err != nil {
				return err
			}
			return a.finish(cmd, out, f.send)
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&in.Account, "account", "a", "", "account to send from (needed with several)")
	fl.StringArrayVar(&in.To, "to", nil, "recipient (repeatable)")
	fl.StringArrayVar(&in.Cc, "cc", nil, "copy to (repeatable)")
	fl.StringArrayVar(&in.Bcc, "bcc", nil, "blind copy to (repeatable)")
	fl.StringVarP(&in.Subject, "subject", "s", "", "subject")
	fl.StringArrayVar(&in.Attach, "attach", nil, "file to attach (repeatable)")
	f.bind(cmd)
	return cmd
}

func newMsgReplyCmd(a *app) *cobra.Command {
	var in ops.DraftReplyIn
	var f composeFlags
	cmd := &cobra.Command{
		Use:   "reply <id>",
		Short: ops.DraftReply.Summary,
		Long: ops.DraftReply.Summary + ": to its Reply-To, else its sender. --all also answers the\n" +
			"other recipients; your own addresses are never added. --body-file text goes above the quote." + composeHelp,
		Example: "  nou msg reply a3f9c2e1b0d4\n  echo 'Thanks!' | nou msg reply a3f9c2e1b0d4 --body-file - --send",
		Args:    exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.ID = args[0]
			var err error
			if in.Body, err = f.body(cmd); err != nil {
				return err
			}
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.DraftReply, in)
			if err != nil {
				return err
			}
			return a.finish(cmd, out, f.send)
		},
	}
	cmd.Flags().BoolVar(&in.All, "all", false, "reply to all recipients")
	cmd.Flags().StringVarP(&in.Account, "account", "a", "", "account, if the id exists in several")
	f.bind(cmd)
	return cmd
}

func newMsgForwardCmd(a *app) *cobra.Command {
	var in ops.DraftForwardIn
	var f composeFlags
	cmd := &cobra.Command{
		Use:     "forward <id>",
		Short:   ops.DraftForward.Summary,
		Long:    ops.DraftForward.Summary + ", inline, with its attachments." + composeHelp,
		Example: "  nou msg forward a3f9c2e1b0d4 --to bob@example.org --send",
		Args:    exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.ID = args[0]
			var err error
			if in.Body, err = f.body(cmd); err != nil {
				return err
			}
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.DraftForward, in)
			if err != nil {
				return err
			}
			return a.finish(cmd, out, f.send)
		},
	}
	cmd.Flags().StringArrayVar(&in.To, "to", nil, "recipient (repeatable)")
	cmd.Flags().StringVarP(&in.Account, "account", "a", "", "account, if the id exists in several")
	f.bind(cmd)
	return cmd
}

// finish ends a command that started a draft: it sends it (--send), opens
// the editor (in a terminal), or reports the draft (scripts).
func (a *app) finish(cmd *cobra.Command, out ops.DraftOut, send bool) error {
	switch {
	case send:
		return a.sendNow(cmd, out.ID)
	case a.interactive() && !a.json:
		return a.editLoop(cmd, out)
	default:
		return a.emit(cmd, out, func(w io.Writer) error { return printDraftLine(w, out) })
	}
}

func printDraftLine(w io.Writer, out ops.DraftOut) error {
	if err := printf(w, "draft %s %s\n", out.ID, out.Path); err != nil {
		return err
	}
	return printProblems(w, out.Problems)
}

func printProblems(w io.Writer, problems []compose.Problem) error {
	for _, p := range problems {
		label := "check"
		if p.Blocking {
			label = "cannot send"
		}
		if err := printf(w, "%s: %s\n", label, sanitize(p.Text)); err != nil {
			return err
		}
	}
	return nil
}

// sendNow sends a draft at once: the CLI has no undo window. A message the
// server will be asked again exits 3 (partial); a refused one exits 1.
func (a *app) sendNow(cmd *cobra.Command, draftID string) error {
	e, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.MsgSend, ops.SendIn{DraftID: draftID, Now: true})
	if err != nil {
		return fmt.Errorf("%w (draft %s kept)", err, draftID)
	}
	return a.flushAndReport(cmd, e)
}

// flushAndReport sends what is due and reports what became of e.
func (a *app) flushAndReport(cmd *cobra.Command, e ops.OutboxEntry) error {
	ctx := cmd.Context()
	flushed, flushErr := ops.Call(ctx, a.env, callerPerm, ops.OutboxFlush, ops.FlushIn{})
	pending, err := ops.Call(ctx, a.env, callerPerm, ops.OutboxList, struct{}{})
	if err != nil {
		return errors.Join(flushErr, err)
	}
	e.State = ops.StateSent // gone from the outbox
	if i := slices.IndexFunc(pending, func(p ops.OutboxEntry) bool { return p.ID == e.ID }); i >= 0 {
		e = pending[i]
	}
	if err := a.emit(cmd, e, func(w io.Writer) error {
		if e.State != ops.StateSent {
			return nil
		}
		return printf(w, "sent: %s\n", sanitize(e.Subject))
	}); err != nil {
		return err
	}
	a.notes(cmd, flushed.Notes)
	switch e.State {
	case ops.StateSent:
		return flushErr
	case ops.StateFailed:
		return fmt.Errorf("the server refused %q: %s (nou outbox retry or nou outbox cancel %s)", e.Subject, e.LastError, e.ID)
	default:
		return fmt.Errorf("%q was not sent (%s); it will be retried at the next sync or nou outbox send: %w",
			e.Subject, e.LastError, ops.ErrPartial)
	}
}

// notes prints remarks from an op on stderr.
func (a *app) notes(cmd *cobra.Command, notes []string) {
	for _, n := range notes {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "nou: %s\n", sanitize(n))
	}
}

// editLoop is the terminal flow: the editor, the checks, then a choice.
func (a *app) editLoop(cmd *cobra.Command, out ops.DraftOut) error {
	ctx, id := cmd.Context(), out.ID
	p := &prompter{a: a, ctx: ctx, in: bufio.NewReader(a.opts.Stdin), w: cmd.OutOrStdout()}
	for {
		if err := a.edit(ctx, out.Path); err != nil {
			return fmt.Errorf("%w (draft %s kept)", err, id)
		}
		var err error
		if out, err = ops.Call(ctx, a.env, callerPerm, ops.DraftUpdate, ops.DraftIDIn{ID: id}); err != nil {
			return fmt.Errorf("%w (draft %s kept: nou draft edit %s)", err, id, id)
		}
		again, err := a.choose(cmd, p, out)
		if errors.Is(err, errQuit) {
			return p.say("Draft %s kept.", id)
		}
		if err != nil || !again {
			return err
		}
	}
}

// choose asks what to do with an edited draft; again means edit it again.
func (a *app) choose(cmd *cobra.Command, p *prompter, out ops.DraftOut) (again bool, err error) {
	if err := printProblems(p.w, out.Problems); err != nil {
		return false, err
	}
	for {
		choice, err := p.ask("[s]end, [e]dit, keep as [d]raft, or discard and [q]uit", "")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(choice) {
		case "s":
			err := a.sendNow(cmd, out.ID)
			if !errors.Is(err, ops.ErrUsage) {
				return false, err
			}
			if err := p.say("%s", sanitize(err.Error())); err != nil {
				return false, err
			}
		case "e":
			return true, nil
		case "d":
			return false, p.say("Draft %s kept.", out.ID)
		case "q":
			if _, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.DraftDiscard, ops.DraftIDIn{ID: out.ID}); err != nil {
				return false, err
			}
			return false, p.say("Discarded.")
		}
	}
}

// editorCommand is the user's editor: $VISUAL, then $EDITOR, else vi
// (notepad on Windows). Like git, it may carry arguments ("code --wait").
func (a *app) editorCommand() []string {
	fallback := "vi"
	if runtime.GOOS == "windows" {
		fallback = "notepad"
	}
	return strings.Fields(cmp.Or(strings.TrimSpace(a.opts.Getenv("VISUAL")), strings.TrimSpace(a.opts.Getenv("EDITOR")), fallback))
}

// edit opens the user's editor on a file and waits for it to close.
func (a *app) edit(ctx context.Context, path string) error {
	if a.editor != nil {
		return a.editor(ctx, path)
	}
	args := a.editorCommand()
	c := exec.CommandContext(ctx, args[0], append(args[1:], path)...) //nolint:gosec // G204: the user's own editor
	c.Stdin, c.Stdout, c.Stderr = a.opts.Stdin, a.opts.Stdout, a.opts.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("editor %s: %w", args[0], err)
	}
	return nil
}
