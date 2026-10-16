// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package ops

import (
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/pluja/nought/internal/compose"
)

// A FIFO or a device would hang the send (reading never ends): only
// regular files can be attached.
func TestAttachOnlyRegularFiles(t *testing.T) {
	env := draftEnv(t)
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	d, err := Call(t.Context(), env, Draft, DraftNew, DraftNewIn{To: []string{"a@x.org"}, Subject: "s", Attach: []string{fifo}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(d.Problems, func(p compose.Problem) bool { return p.Blocking && strings.Contains(p.Text, "regular file") }) {
		t.Errorf("problems = %+v", d.Problems)
	}
}
