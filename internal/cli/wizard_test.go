// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"nought/internal/autoconfig"
	"nought/internal/config"
	"nought/internal/ops"
	"nought/internal/secret"
)

// The online branch of the wizard, with a fake network.
func TestWizardDiscoverOnline(t *testing.T) {
	home := t.TempDir()
	paths, err := config.ResolvePaths("linux", func(k string) string {
		if k == "NOUGHT_HOME" {
			return home
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	env := ops.NewEnv(paths, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now, ops.BuildInfo{}, secret.Memory{})
	env.Network = autoconfig.Network{
		LookupSRV: func(_ context.Context, service, _, name string) (string, []*net.SRV, error) {
			if service == "imaps" {
				return "", []*net.SRV{{Target: "imap." + name + ".", Port: 993}}, nil
			}
			return "", nil, errors.New("none")
		},
	}
	a := &app{env: env}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())

	for _, tt := range []struct {
		answer   string
		wantHost string
	}{
		{"y\n", "imap.custom.example"},
		{"n\n", ""},
	} {
		var out strings.Builder
		p := &prompter{a: a, ctx: t.Context(), in: bufio.NewReader(strings.NewReader(tt.answer)), w: &out}
		res, err := a.discover(cmd, p, "me@custom.example")
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if res != nil {
			got = res.IMAP.Host
		}
		if got != tt.wantHost {
			t.Errorf("answer %q: host %q, want %q\n%s", tt.answer, got, tt.wantHost, out.String())
		}
	}
}

func TestSuggestName(t *testing.T) {
	for _, tt := range []struct {
		email string
		taken map[string]bool
		want  string
	}{
		{"me@posteo.de", nil, "posteo"},
		{"me@posteo.de", map[string]bool{"posteo": true}, "posteo-2"},
		{"me@Ünïcode.example", nil, "ncode"},
		{"me@_.example", nil, "mail"},
	} {
		if got := suggestName(tt.email, tt.taken); got != tt.want {
			t.Errorf("suggestName(%q) = %q, want %q", tt.email, got, tt.want)
		}
	}
}
