// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build unix

package ops

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/pluja/nought/internal/compose"
	"github.com/pluja/nought/internal/smtptest"
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

// A file that cannot be read is not queued: the message would go without it.
func TestUnreadableAttachmentIsNotQueued(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything")
	}
	env, srv := sendEnv(t, smtptest.Options{})
	file := filepath.Join(t.TempDir(), "secret.txt")
	writeFile(t, file, "x")
	if err := os.Chmod(file, 0o000); err != nil {
		t.Fatal(err)
	}
	d := newDraft(t, env, "alice@example.org", "File", "attached\n")
	if _, err := Call(t.Context(), env, Draft, DraftSet, DraftSetIn{ID: d.ID, Attach: []string{file}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID}); !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "cannot attach") {
		t.Fatalf("send = %v", err)
	}
	if list, _ := Call(t.Context(), env, Read, OutboxList, struct{}{}); len(list) != 0 || len(srv.Messages()) != 0 {
		t.Errorf("queued %+v, sent %d", list, len(srv.Messages()))
	}
}
