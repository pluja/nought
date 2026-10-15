// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"nought/internal/ops"
)

func newPathsCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "paths",
		Short: ops.PathsShow.Summary,
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.PathsShow, struct{}{})
			if err != nil {
				return err
			}
			return a.emit(cmd, out, func(w io.Writer) error {
				rows := [][2]string{
					{"config", out.ConfigFile},
					{"data", out.DataDir},
					{"archive", out.ArchiveDir},
					{"index", out.IndexFile},
					{"tags dump", out.TagsDump},
					{"journal", out.Journal},
				}
				for _, r := range rows {
					if _, err := fmt.Fprintf(w, "%-10s %s\n", r[0], r[1]); err != nil {
						return err
					}
				}
				if _, err := fmt.Fprintln(w, "\nback up:"); err != nil {
					return err
				}
				for _, p := range out.Backup.Include {
					if _, err := fmt.Fprintf(w, "  %s\n", p); err != nil {
						return err
					}
				}
				_, err := fmt.Fprintf(w, "optional (rebuild with 'nou reindex'):\n  %s\nnever back up (deleted and expiring mail):\n  %s\n",
					out.IndexFile, out.TransientDir)
				return err
			})
		},
	}
}

func newVersionCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:         "version",
		Short:       ops.VersionShow.Summary,
		Annotations: map[string]string{noPaths: "true"},
		Args:        noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := ops.Call(cmd.Context(), a.env, callerPerm, ops.VersionShow, struct{}{})
			if err != nil {
				return err
			}
			return a.emit(cmd, out, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "nou %s (%s)\n", out.Version, out.GoVersion)
				return err
			})
		},
	}
}
