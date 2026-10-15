// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"nought/internal/autoconfig"
	"nought/internal/ops"
)

func newAccountDiscoverCmd(a *app) *cobra.Command {
	var in ops.DiscoverIn
	var online bool
	cmd := &cobra.Command{
		Use:   "discover <email>",
		Short: ops.AccountDiscover.Summary,
		Long: ops.AccountDiscover.Summary + ".\n\n" +
			"Offline, only nought's built-in provider list (a copy of Mozilla's ISPDB) is used.\n" +
			"With --online, nou may also query DNS (SRV and MX records), fetch the provider's\n" +
			"own autoconfig file over HTTPS (without your address in the request), and try\n" +
			"common host names over TLS. Mozilla's online service is never contacted.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Email = args[0]
			op := ops.AccountDiscover
			if online {
				op = ops.AccountDiscoverOnline
			}
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, op, in)
			if err != nil {
				return err
			}
			return a.emit(cmd, out, func(w io.Writer) error { return printDiscovery(w, out) })
		},
	}
	cmd.Flags().BoolVar(&online, "online", false, "allow DNS lookups and contacting the provider")
	return cmd
}

func printDiscovery(w io.Writer, r autoconfig.Result) error {
	name := r.Provider
	if name == "" {
		name = "your provider"
	}
	if err := printf(w, "Found %s (%s)\n  IMAP  %s\n", sanitize(name), r.Source, serverLine(r.IMAP.Host, r.IMAP.Port, r.IMAP.TLS)); err != nil {
		return err
	}
	if r.SMTP.Host != "" {
		if err := printf(w, "  SMTP  %s\n", serverLine(r.SMTP.Host, r.SMTP.Port, r.SMTP.TLS)); err != nil {
			return err
		}
	}
	if err := printf(w, "  login %s\n", sanitize(r.Username)); err != nil {
		return err
	}
	for _, c := range r.Contacted {
		if err := printf(w, "  contacted: %s\n", sanitize(c)); err != nil {
			return err
		}
	}
	return nil
}

func serverLine(host string, port int, tlsMode string) string {
	mode := "TLS"
	if tlsMode == "starttls" {
		mode = "STARTTLS"
	}
	return sanitize(host) + ":" + strconv.Itoa(port) + " (" + mode + ")"
}
