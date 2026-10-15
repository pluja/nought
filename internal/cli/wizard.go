// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"nought/internal/autoconfig"
	"nought/internal/config"
	"nought/internal/ops"
)

// errQuit ends the wizard without saving.
var errQuit = errors.New("quit")

// prompter asks questions on the terminal.
type prompter struct {
	a  *app
	in *bufio.Reader
	w  io.Writer
}

// ask shows a question with a default and returns the answer (or the
// default for an empty answer).
func (p *prompter) ask(question, def string) (string, error) {
	if def != "" {
		question += " [" + def + "]"
	}
	if _, err := fmt.Fprintf(p.w, "%s: ", question); err != nil {
		return "", err
	}
	line, err := p.in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", errQuit // end of input: stop without saving
	}
	if answer := strings.TrimSpace(line); answer != "" {
		return answer, nil
	}
	return def, nil
}

// yes asks a yes/no question.
func (p *prompter) yes(question string, def bool) (bool, error) {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	answer, err := p.ask(question+" ("+d+")", "")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(answer) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// secret reads a password without echo on a terminal.
func (p *prompter) secret(question string) (string, error) {
	if _, err := fmt.Fprintf(p.w, "%s: ", question); err != nil {
		return "", err
	}
	if f, ok := p.a.opts.Stdin.(*os.File); ok && isTerminal(f) {
		pw, err := term.ReadPassword(int(f.Fd())) //nolint:gosec // G115: file descriptors fit in int
		_, _ = fmt.Fprintln(p.w)
		return string(pw), err
	}
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", errQuit
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (p *prompter) say(format string, args ...any) error {
	_, err := fmt.Fprintf(p.w, format+"\n", args...)
	return err
}

// accountWizard walks the user through adding an account. Nothing is saved
// until they confirm at the end.
func (a *app) accountWizard(cmd *cobra.Command) error {
	p := &prompter{a: a, in: bufio.NewReader(a.opts.Stdin), w: cmd.OutOrStdout()}
	err := a.runWizard(cmd, p)
	if errors.Is(err, errQuit) {
		_ = p.say("Nothing was saved.")
		return nil
	}
	return err
}

func (a *app) runWizard(cmd *cobra.Command, p *prompter) error {
	ctx := cmd.Context()
	if err := p.say("Add a mail account. Nothing is saved until you confirm.\n"); err != nil {
		return err
	}

	var in ops.AccountIn
	for {
		email, err := p.ask("Email address", "")
		if err != nil {
			return err
		}
		if _, perr := mail.ParseAddress(email); perr == nil && !strings.ContainsAny(email, "<>") {
			in.Email = email
			break
		}
		if err := p.say("  That is not an email address like me@example.org."); err != nil {
			return err
		}
	}

	found, err := a.discover(cmd, p, in.Email)
	if err != nil {
		return err
	}
	if found != nil {
		in.IMAPHost, in.IMAPPort, in.IMAPTLS = found.IMAP.Host, found.IMAP.Port, found.IMAP.TLS
		in.SMTPHost, in.SMTPPort, in.SMTPTLS = found.SMTP.Host, found.SMTP.Port, found.SMTP.TLS
		in.Username = found.Username
		ok, err := p.yes("Use these settings?", true)
		if err != nil {
			return err
		}
		if !ok {
			found = nil
		}
	}
	if found == nil {
		if err := editServers(p, &in); err != nil {
			return err
		}
	}

	if in.Name, err = askName(cmd, a, p, in.Email); err != nil {
		return err
	}
	if in.Username == "" {
		in.Username = in.Email
	}
	if in.Username, err = p.ask("Login name", in.Username); err != nil {
		return err
	}
	if in.Username == in.Email {
		in.Username = "" // the default; keeps the config short
	}
	if err := askPassword(p, &in); err != nil {
		return err
	}

	if err := a.checkUntilAccepted(cmd, p, &in); err != nil {
		return err
	}
	ok, err := p.yes(fmt.Sprintf("\nSave account %q?", in.Name), true)
	if err != nil {
		return err
	}
	if !ok {
		return errQuit
	}
	if _, err := ops.Call(ctx, a.env, callerPerm, ops.AccountAdd, in); err != nil {
		return err
	}
	return p.say("Saved %s. Server deletion is off.\nNext: nou sync %s   (archives your mail; nothing is deleted)", in.Name, in.Name)
}

// checkUntilAccepted tests the settings until they work or the user
// chooses to save anyway (nil) or to quit (errQuit).
func (a *app) checkUntilAccepted(cmd *cobra.Command, p *prompter, in *ops.AccountIn) error {
	for {
		if err := p.say("\nTesting the connection…"); err != nil {
			return err
		}
		out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.AccountCheck, *in)
		if err == nil {
			return printCheck(p, out)
		}
		if err := p.say("  Could not connect: %s", sanitize(err.Error())); err != nil {
			return err
		}
		choice, err := p.ask("[r]etry, [e]dit settings, [s]ave anyway, or [q]uit", "r")
		if err != nil {
			return err
		}
		switch strings.ToLower(choice) {
		case "e", "edit":
			if err := editServers(p, in); err != nil {
				return err
			}
			if err := askPassword(p, in); err != nil {
				return err
			}
		case "s", "save":
			return nil
		case "q", "quit":
			return errQuit
		}
	}
}

// discover tries the built-in provider list, then asks before going online.
func (a *app) discover(cmd *cobra.Command, p *prompter, email string) (*autoconfig.Result, error) {
	res, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.AccountDiscover, ops.DiscoverIn{Email: email})
	if errors.Is(err, ops.ErrNotFound) {
		domain := email[strings.LastIndex(email, "@")+1:]
		if err := p.say("Not in nought's built-in provider list."); err != nil {
			return nil, err
		}
		online, askErr := p.yes("Look up the settings online? This queries DNS and contacts "+domain+"'s servers", false)
		if askErr != nil || !online {
			return nil, askErr
		}
		res, err = ops.Call(cmd.Context(), a.env, callerPerm, ops.AccountDiscover, ops.DiscoverIn{Email: email, Online: true})
	}
	switch {
	case errors.Is(err, ops.ErrNotFound):
		return nil, p.say("No settings found; please enter them.")
	case err != nil:
		return nil, p.say("  %s\n  Please enter the settings yourself.", sanitize(err.Error()))
	}
	if err := printDiscovery(p.w, res); err != nil {
		return nil, err
	}
	return &res, nil
}

// editServers asks for the IMAP (and optional SMTP) settings.
func editServers(p *prompter, in *ops.AccountIn) error {
	domain := in.Email[strings.LastIndex(in.Email, "@")+1:]
	knownBefore := in.IMAPHost != ""
	var err error
	if in.IMAPHost, err = p.ask("IMAP server", fallback(in.IMAPHost, "imap."+domain)); err != nil {
		return err
	}
	if in.IMAPTLS, in.IMAPPort, err = askSecurity(p, "IMAP", in.IMAPTLS, in.IMAPPort, 993, 143); err != nil {
		return err
	}
	// Suggest smtp.<domain> only the first time; afterwards keep "none".
	smtpDefault := "smtp." + domain
	if in.SMTPHost != "" {
		smtpDefault = in.SMTPHost
	} else if knownBefore {
		smtpDefault = "-"
	}
	if in.SMTPHost, err = p.ask("SMTP server for sending (optional; '-' for none)", smtpDefault); err != nil {
		return err
	}
	if in.SMTPHost == "-" {
		in.SMTPHost, in.SMTPPort, in.SMTPTLS = "", 0, ""
		return nil
	}
	in.SMTPTLS, in.SMTPPort, err = askSecurity(p, "SMTP", in.SMTPTLS, in.SMTPPort, 465, 587)
	return err
}

func askSecurity(p *prompter, what, current string, port, implicitPort, starttlsPort int) (string, int, error) {
	def := "tls"
	if current == config.TLSStartTLS {
		def = "starttls"
	}
	for {
		answer, err := p.ask(what+" security: tls or starttls", def)
		if err != nil {
			return "", 0, err
		}
		mode := config.TLSImplicit
		switch strings.ToLower(answer) {
		case "tls", "ssl", "implicit":
		case "starttls":
			mode = config.TLSStartTLS
		default:
			if err := p.say("  Please answer tls or starttls (plaintext is not supported)."); err != nil {
				return "", 0, err
			}
			continue
		}
		defPort := implicitPort
		if mode == config.TLSStartTLS {
			defPort = starttlsPort
		}
		if port != 0 && mode == current {
			defPort = port
		}
		portText, err := p.ask(what+" port", strconv.Itoa(defPort))
		if err != nil {
			return "", 0, err
		}
		n, perr := strconv.Atoi(portText)
		if perr != nil || n < 1 || n > 65535 {
			if err := p.say("  Please give a port number."); err != nil {
				return "", 0, err
			}
			continue
		}
		return mode, n, nil
	}
}

// askName suggests a name from the domain ("posteo" for posteo.de) and
// makes sure it is valid and not taken.
func askName(cmd *cobra.Command, a *app, p *prompter, email string) (string, error) {
	accounts, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.AccountList, struct{}{})
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, acct := range accounts {
		taken[acct.Name] = true
	}
	def := suggestName(email, taken)
	for {
		name, err := p.ask("Account name (short; used in commands)", def)
		if err != nil {
			return "", err
		}
		switch {
		case config.ValidateAccountName(name) != nil:
			err = p.say("  Use 1-32 characters: a-z, 0-9, '-' or '_'.")
		case taken[name]:
			err = p.say("  %s already exists.", name)
		default:
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
}

func suggestName(email string, taken map[string]bool) string {
	domain := strings.ToLower(email[strings.LastIndex(email, "@")+1:])
	base := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return -1
	}, strings.Split(domain, ".")[0])
	if config.ValidateAccountName(base) != nil {
		base = "mail"
	}
	if len(base) > 28 {
		base = base[:28]
	}
	name := base
	for i := 2; taken[name]; i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}

func askPassword(p *prompter, in *ops.AccountIn) error {
	choice, err := p.ask("Store the password in your OS keyring, or use a command that prints it? [k]eyring/[c]ommand", "k")
	if err != nil {
		return err
	}
	if strings.HasPrefix(strings.ToLower(choice), "c") {
		in.Password = ""
		in.PasswordCommand, err = p.ask("Password command (e.g. pass show mail/me)", in.PasswordCommand)
		return err
	}
	in.PasswordCommand = ""
	for {
		pw, err := p.secret("Password (not shown)")
		if err != nil {
			return err
		}
		if pw != "" {
			in.Password = pw
			return nil
		}
	}
}

func printCheck(p *prompter, out ops.AccountTestOut) error {
	if err := p.say("  Connected. Folders:"); err != nil {
		return err
	}
	for _, f := range out.Folders {
		what := "ignore"
		if f.Policy.Action == config.ActionArchive {
			what = fmt.Sprintf("archive, keep %d days on server", f.Policy.KeepDays)
		}
		if err := p.say("    %-24s %s", sanitize(f.Folder), what); err != nil {
			return err
		}
	}
	if !out.UIDPlus {
		return p.say("  Note: the server lacks UIDPLUS, so nou will archive but never delete there.")
	}
	return nil
}

func fallback(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func itoa(n int) string { return strconv.Itoa(n) }
