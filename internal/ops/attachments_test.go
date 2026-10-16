// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// attachmentMail archives a message with two attachments, one with a
// hostile name, and returns its id.
func attachmentMail(t *testing.T, env *Env) string {
	t.Helper()
	raw := "From: a@example.org\r\nMessage-ID: <files@x>\r\nSubject: two files\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nsee files\r\n" +
		"--b\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"../../.bashrc\"\r\n\r\nevil\r\n" +
		"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=report.pdf\r\n\r\n%PDF\r\n--b--\r\n"
	if _, _, err := archive(t.Context(), env, "personal", []byte(raw), "S", nil); err != nil {
		t.Fatal(err)
	}
	return idOf(t, env, `subject:"two files"`)
}

func TestSaveAttachments(t *testing.T) {
	env := draftEnv(t)
	id := attachmentMail(t, env)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.pdf"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := Call(t.Context(), env, Tag, MsgSave, SaveIn{ID: id, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range saved {
		names = append(names, f.Name)
		if filepath.Dir(f.Path) != dir {
			t.Errorf("%s escaped %s", f.Path, dir)
		}
	}
	// The hostile name stays in the directory; nothing is overwritten.
	if !slices.Equal(names, []string{"bashrc", "report (1).pdf"}) {
		t.Errorf("saved %q", names)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "report.pdf")); string(b) != "mine" {
		t.Error("an existing file was overwritten")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "report (1).pdf")); string(b) != "%PDF" {
		t.Errorf("saved content %q", b)
	}
	if info, err := os.Stat(saved[1].Path); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v %v", info, err)
	}

	one, err := Call(t.Context(), env, Tag, MsgSave, SaveIn{ID: id, Dir: filepath.Join(dir, "new"), Only: []int{2}})
	if err != nil || len(one) != 1 || one[0].Name != "report.pdf" {
		t.Errorf("only #2: %+v, %v", one, err)
	}
	for _, in := range []SaveIn{
		{ID: id, Dir: dir, Only: []int{3}},
		{ID: id},
		{ID: idOf(t, env, `subject:"read one"`), Dir: dir},
	} {
		if _, err := Call(t.Context(), env, Tag, MsgSave, in); !errors.Is(err, ErrUsage) {
			t.Errorf("%+v: %v", in, err)
		}
	}
}

func TestSafeFileName(t *testing.T) {
	long := strings.Repeat("a", 300) + ".pdf"
	tests := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\x\a.doc`, "a.doc"},
		{"", "attachment-3"},
		{"..", "attachment-3"},
		{"CON.txt", "_CON.txt"},
		{"nul", "_nul"},
		{"what?:*.txt", "what___.txt"},
		{"invoice\u202Etxt.exe", "invoicetxt.exe"},
		{" .hidden ", "hidden"},
	}
	for _, tt := range tests {
		if got := safeFileName(tt.in, 3); got != tt.want {
			t.Errorf("safeFileName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := safeFileName(long, 1); len(got) > 200 || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("long name: %d bytes, %q", len(got), got[len(got)-8:])
	}
}
