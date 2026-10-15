// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"nought/internal/ops"
	"nought/internal/render"
)

func newAccountCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "account",
		Short: "Manage mail accounts",
		Args:  noArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newAccountAddCmd(a), newAccountListCmd(a), newAccountShowCmd(a),
		newAccountSetCmd(a), newAccountRemoveCmd(a), newAccountTestCmd(a))
	return cmd
}

func newAccountAddCmd(a *app) *cobra.Command {
	var in ops.AccountIn
	var passwordStdin bool
	cmd := &cobra.Command{
		Use:   "add <name> --email ADDRESS --imap-host HOST",
		Short: ops.AccountAdd.Summary,
		Long: ops.AccountAdd.Summary + ".\n\n" +
			"The password goes to the OS keyring: you are prompted for it, or it is read\n" +
			"from stdin with --password-stdin. On headless systems use --password-command\n" +
			"(e.g. 'pass show mail/personal'); its first output line is the password.\n" +
			"Passwords are never accepted as flag values and never written to the config.\n\n" +
			"Server deletion starts disabled. Check the archive after the first sync, then\n" +
			"enable it with: nou account set <name> --deletion enabled",
		Example: "  nou account add personal --email me@example.org --imap-host imap.example.org\n" +
			"  pass show mail/work | nou account add work --email me@corp.example --imap-host mail.corp.example --password-stdin",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Name = args[0]
			if in.PasswordCommand == "" {
				pw, err := a.readPassword(cmd, in.Name, passwordStdin)
				if err != nil {
					return err
				}
				in.Password = pw
			}
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.AccountAdd, in)
			if err != nil {
				return err
			}
			return a.emit(cmd, out, func(w io.Writer) error {
				return printf(w, "added %s; run 'nou sync %s' to archive its mail (server deletion is disabled)\n", out.Name, out.Name)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&in.Email, "email", "", "email address (required)")
	f.StringVar(&in.Username, "username", "", "login name, if not the email address")
	f.StringVar(&in.Label, "label", "", "short label shown in lists")
	f.StringVar(&in.Color, "color", "", "color like #44aa99")
	f.StringVar(&in.IMAPHost, "imap-host", "", "IMAP server (required)")
	f.IntVar(&in.IMAPPort, "imap-port", 0, "IMAP port (default 993, or 143 with starttls)")
	f.StringVar(&in.IMAPTLS, "imap-tls", "implicit", "implicit or starttls")
	f.StringVar(&in.SMTPHost, "smtp-host", "", "SMTP server")
	f.IntVar(&in.SMTPPort, "smtp-port", 0, "SMTP port (default 465, or 587 with starttls)")
	f.StringVar(&in.SMTPTLS, "smtp-tls", "implicit", "implicit or starttls")
	f.StringVar(&in.PasswordCommand, "password-command", "", "command printing the password (stored in the config: never put the password itself in it)")
	f.BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin")
	return cmd
}

// maxPassword bounds what is read as a password.
const maxPassword = 4096

// readPassword gets a password from stdin or, on a terminal, by prompting
// without echo. It never prompts when stdin is not a terminal.
func (a *app) readPassword(cmd *cobra.Command, account string, fromStdin bool) (string, error) {
	if fromStdin {
		line, err := bufio.NewReader(io.LimitReader(a.opts.Stdin, maxPassword)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read password: %w", err)
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	f, ok := a.opts.Stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) { //nolint:gosec // G115: file descriptors fit in int
		return "", fmt.Errorf("stdin is not a terminal: use --password-stdin or --password-command: %w", ops.ErrUsage)
	}
	if err := printf(cmd.ErrOrStderr(), "Password for %s: ", account); err != nil {
		return "", err
	}
	pw, err := term.ReadPassword(int(f.Fd())) //nolint:gosec // G115: file descriptors fit in int
	_ = printf(cmd.ErrOrStderr(), "\n")
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(pw), nil
}

func newAccountListCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: ops.AccountList.Summary,
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.AccountList, struct{}{})
			return a.result(cmd, out, err, func(w io.Writer) error {
				for _, acct := range out {
					if err := printf(w, "%-12s %-30s %s:%d  deletion %s\n", acct.Name, sanitize(acct.Email),
						sanitize(acct.IMAP.Host), acct.IMAP.Port, acct.Deletion); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}

func newAccountShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: ops.AccountShow.Summary,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.AccountShow, ops.AccountName{Name: args[0]})
			return a.result(cmd, out, err, func(w io.Writer) error { return printAccount(w, out) })
		},
	}
}

func printAccount(w io.Writer, acct ops.AccountOut) error {
	lines := []string{
		"name:      " + acct.Name,
		"email:     " + acct.Email,
		"username:  " + acct.Username,
		fmt.Sprintf("imap:      %s:%d (%s)", acct.IMAP.Host, acct.IMAP.Port, acct.IMAP.TLS),
	}
	if acct.SMTP != nil {
		lines = append(lines, fmt.Sprintf("smtp:      %s:%d (%s)", acct.SMTP.Host, acct.SMTP.Port, acct.SMTP.TLS))
	}
	lines = append(lines, "password:  from "+acct.PasswordSource, "deletion:  "+acct.Deletion, "archive:   "+acct.Archive, "folders:")
	for _, r := range acct.Folders {
		rule := "  " + r.Match + " -> "
		switch {
		case r.Action == "ignore":
			rule += "ignore"
		case r.KeepDays != nil:
			rule += fmt.Sprintf("archive, keep %d days on server", *r.KeepDays)
		default:
			rule += "archive, keep 7 days on server"
		}
		lines = append(lines, rule)
	}
	lines = append(lines, "  (other folders) -> archive, keep 7 days on server")
	for i := range lines {
		lines[i] = render.Sanitize(lines[i])
	}
	return printf(w, "%s\n", strings.Join(lines, "\n"))
}

func newAccountSetCmd(a *app) *cobra.Command {
	var in ops.AccountSetIn
	cmd := &cobra.Command{
		Use:     "set <name>",
		Short:   ops.AccountSet.Summary,
		Example: "  nou account set personal --deletion enabled",
		Args:    exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Name = args[0]
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.AccountSet, in)
			return a.result(cmd, out, err, func(w io.Writer) error { return printAccount(w, out) })
		},
	}
	cmd.Flags().StringVar(&in.Deletion, "deletion", "", "enabled or disabled: delete archived mail from the server per folder policy")
	cmd.Flags().StringVar(&in.Label, "label", "", "short label")
	cmd.Flags().StringVar(&in.Color, "color", "", "color like #44aa99")
	return cmd
}

func newAccountRemoveCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: ops.AccountRemove.Summary,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.AccountRemove, ops.AccountName{Name: args[0]})
			return a.result(cmd, out, err, func(w io.Writer) error {
				return printf(w, "removed %s; its archive is kept at %s\n", out.Name, out.Archive)
			})
		},
	}
}
