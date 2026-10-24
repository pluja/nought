// SPDX-License-Identifier: AGPL-3.0-or-later

package importer

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

type got struct{ folder, flags, subject string }

func collect(t *testing.T, items func(func(Item, error) bool)) ([]got, []error) {
	t.Helper()
	var out []got
	var errs []error
	for item, err := range items {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if item.Deleted {
			continue
		}
		subj := headerValue(item.Raw, "Subject")
		out = append(out, got{item.Folder, item.Flags, strings.TrimSpace(subj)})
	}
	slices.SortFunc(out, func(a, b got) int { return strings.Compare(a.subject, b.subject) })
	return out, errs
}

func TestMaildirTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "export")
	write(t, filepath.Join(root, "cur", maildirName("1", "S")), "Subject: inbox read\n\nx")
	write(t, filepath.Join(root, "new", "2"), "Subject: inbox new\n\nx")
	write(t, filepath.Join(root, "tmp", "3"), "Subject: in tmp\n\nx")
	write(t, filepath.Join(root, "Work", "Projects", "cur", "4;2,FR"), "Subject: nested\n\nx")
	write(t, filepath.Join(root, "Saved", "a.eml"), "Subject: eml file\n\nx")
	write(t, filepath.Join(root, "Saved", "notes.txt"), "not mail")
	write(t, filepath.Join(root, ".hidden", "a.eml"), "Subject: hidden\n\nx")
	write(t, filepath.Join(root, ".INBOX.Sent", "cur", maildirName("8", "S")), "Subject: maildir++\n\nx")
	write(t, filepath.Join(root, "Saved", "tmp", "b.eml"), "Subject: eml in tmp\n\nx")
	write(t, filepath.Join(root, "Spoof", "cur", "9"), "Subject: spoof\nX-Mozilla-Status: 0008\n\nx")
	write(t, filepath.Join(root, "Trash", "cur", "6"), "X-Mozilla-Status: 0009\nSubject: deleted\n\nx")
	write(t, filepath.Join(root, "Thunder", "cur", "7"), "X-Mozilla-Status: 0005\nSubject: mozilla flags\n\nx")

	items, errs := collect(t, Maildir(root))
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	want := []got{
		{"Saved", "", "eml file"},
		{"Saved/tmp", "", "eml in tmp"},
		{"export", "", "inbox new"},
		{"export", "S", "inbox read"},
		{"INBOX/Sent", "S", "maildir++"},
		{"Thunder", "SF", "mozilla flags"},
		{"Work/Projects", "FR", "nested"},
		{"Spoof", "", "spoof"}, // X-Mozilla-Status not first: from the sender, ignored
	}
	slices.SortFunc(want, func(a, b got) int { return strings.Compare(a.subject, b.subject) })
	if !slices.Equal(items, want) {
		t.Errorf("items =\n%v\nwant\n%v", items, want)
	}
}

func TestMaildirSymlinksAndErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.eml")
	write(t, outside, "Subject: outside\n\nx")
	if err := os.Symlink(outside, filepath.Join(root, "link.eml")); err != nil {
		t.Fatal(err)
	}
	items, errs := collect(t, Maildir(root))
	if len(items) != 0 || len(errs) != 0 {
		t.Errorf("followed symlink: %v %v", items, errs)
	}

	_, errs = collect(t, Maildir(filepath.Join(root, "missing")))
	if len(errs) != 1 {
		t.Errorf("missing root errors = %v", errs)
	}
}

func TestMaildirEarlyStop(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"a.eml", "b.eml", "c.eml"} {
		write(t, filepath.Join(root, n), "Subject: x\n\n")
	}
	n := 0
	for _, err := range Maildir(root) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		break
	}
	if n != 1 {
		t.Errorf("n = %d", n)
	}
}

const thunderbirdMbox = "From - Tue Oct 14 09:30:00 2026\r\n" +
	"X-Mozilla-Status: 0001\r\n" +
	"Subject: first\r\n" +
	"\r\n" +
	">From the start of a line\r\n" +
	">>From stays quoted once\r\n" +
	"From inside a paragraph is not a separator\r\n" +
	"\r\n" +
	"From - Tue Oct 14 10:00:00 2026\r\n" +
	"X-Mozilla-Status: 0008\r\n" +
	"Subject: expunged\r\n" +
	"\r\n" +
	"gone\r\n" +
	"\r\n" +
	"From - Tue Oct 14 11:00:00 2026\r\n" +
	"Subject: last\r\n" +
	"\r\n" +
	"end\r\n"

func TestMbox(t *testing.T) {
	var raws []string
	var deleted int
	for item, err := range Mbox(strings.NewReader(thunderbirdMbox), "Inbox", "inbox.mbox") {
		if err != nil {
			t.Fatal(err)
		}
		if item.Deleted {
			deleted++
			continue
		}
		raws = append(raws, string(item.Raw))
	}
	want := []string{
		"X-Mozilla-Status: 0001\r\nSubject: first\r\n\r\nFrom the start of a line\r\n>From stays quoted once\r\nFrom inside a paragraph is not a separator\r\n",
		"Subject: last\r\n\r\nend\r\n",
	}
	if !slices.Equal(raws, want) {
		t.Errorf("messages =\n%q\nwant\n%q", raws, want)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d", deleted)
	}
	items, _ := collect(t, Mbox(strings.NewReader(thunderbirdMbox), "Inbox", "x"))
	if items[0].flags != "S" || items[0].folder != "Inbox" {
		t.Errorf("first item = %+v", items[0])
	}
}

func TestMboxEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		count   int
		wantErr bool
	}{
		{"empty", "", 0, false},
		{"blank lines only", "\n\n", 0, false},
		{"not mbox", "Subject: x\n\nbody", 0, true},
		{"lf endings no trailing newline", "From a@b Tue Oct 14 09:30:00 2026\nSubject: x\n\nbody", 1, false},
		{"two messages lf", "From a@b x\nSubject: 1\n\nx\n\nFrom - x\nSubject: 2\n\ny\n", 2, false},
		{"separator without blank line", "From a@b x\nSubject: 1\n\nx\nFrom b@c Tue Oct 14 09:30:00 2026\nSubject: 2\n\ny\n", 2, false},
		{"body From after blank line is not a separator", "From a@b x\nSubject: 1\n\nx\n\nFrom here on\n", 1, false},
		{"internal data message skipped", "From MAILER-DAEMON x\nFrom: Mail System Internal Data <MAILER-DAEMON@host>\n" +
			"Subject: DON'T DELETE THIS MESSAGE -- FOLDER INTERNAL DATA\nX-IMAP: 1234567890 0000000001\n\nx\n\n" +
			"From MAILER-DAEMON x\nSubject: real\n\ny\n", 1, false},
		// Review: any message with that subject was dropped from imports.
		{"a real message with that subject is kept", "From a@b x\nFrom: Bob <bob@example.org>\n" +
			"Subject: FOLDER INTERNAL DATA\n\nx\n", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, errs := 0, 0
			for item, err := range Mbox(strings.NewReader(tt.in), "f", "s") {
				if err != nil {
					errs++
					continue
				}
				if !item.Deleted {
					n++
				}
			}
			if n != tt.count || (errs > 0) != tt.wantErr {
				t.Errorf("count = %d, errors = %d", n, errs)
			}
		})
	}
}

func TestMboxHugeLineIsBounded(t *testing.T) {
	// A line far longer than the buffer must not be held in memory whole.
	huge := strings.NewReader("From a@b Tue Oct 14 09:30:00 2026\nSubject: x\n\n" + strings.Repeat("y", 300<<10) + "\n")
	var items int
	for item, err := range Mbox(huge, "f", "s") {
		if err != nil {
			t.Fatal(err)
		}
		if len(item.Raw) < 300<<10 {
			t.Errorf("message truncated to %d bytes", len(item.Raw))
		}
		items++
	}
	if items != 1 {
		t.Errorf("items = %d", items)
	}
}

func TestReadLineOverflow(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader(strings.Repeat("z", 100)+"\nnext\n"), 16)
	line, overflow, err := readLine(br, 10)
	if err != nil || !overflow || len(line) > 10 {
		t.Fatalf("readLine = %d bytes, %v, %v", len(line), overflow, err)
	}
	line, overflow, err = readLine(br, 10)
	if err != nil || overflow || string(line) != "next\n" {
		t.Fatalf("next line = %q, %v, %v", line, overflow, err)
	}
}

func TestMaildirSymlinkedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	target := t.TempDir()
	write(t, filepath.Join(target, "a.eml"), "Subject: via link\n\nx")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	items, errs := collect(t, Maildir(link))
	if len(errs) != 0 || len(items) != 1 {
		t.Errorf("items = %v, errs = %v", items, errs)
	}
}

func TestDetect(t *testing.T) {
	dir := t.TempDir()
	mbox := filepath.Join(dir, "Inbox")
	write(t, mbox, "From - x\nSubject: a\n\n")
	other := filepath.Join(dir, "other.txt")
	write(t, other, "hello")
	for path, want := range map[string]string{dir: FormatMaildir, mbox: FormatMbox} {
		if got, err := Detect(path); err != nil || got != want {
			t.Errorf("Detect(%s) = %q, %v", path, got, err)
		}
	}
	for _, bad := range []string{other, filepath.Join(dir, "missing")} {
		if _, err := Detect(bad); err == nil {
			t.Errorf("Detect(%s) succeeded", bad)
		}
	}
}

func TestMaildirFlags(t *testing.T) {
	for name, want := range map[string]string{
		"x:2,FS": "FS", "x;2,S": "S", "x!2,RR": "R", "x": "", "x:2,abc": "", "x:2,SXF": "SF",
	} {
		if got := maildirFlags(name); got != want {
			t.Errorf("maildirFlags(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestReadLimitedTooLarge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.eml")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxMessageSize + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := readLimited(path); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v", err)
	}
}

func FuzzMbox(f *testing.F) {
	f.Add(thunderbirdMbox)
	f.Add("From a\n>From b\n\nFrom c\n")
	f.Fuzz(func(t *testing.T, s string) {
		total := 0
		for item, err := range Mbox(strings.NewReader(s), "f", "s") {
			if err == nil {
				total += len(item.Raw)
			}
		}
		if total > len(s) {
			t.Fatalf("output larger than input: %d > %d", total, len(s))
		}
	})
}

// maildirName is a Maildir file name ("unique:2,flags"), with ";" on
// Windows, where ":" is not allowed (NTFS would quietly write the message
// to a hidden stream of the file "unique").
func maildirName(unique, flags string) string {
	sep := ":"
	if runtime.GOOS == "windows" {
		sep = ";"
	}
	return unique + sep + "2," + flags
}
