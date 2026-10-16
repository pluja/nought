// SPDX-License-Identifier: AGPL-3.0-or-later

// Package importer reads messages from other mail stores: Maildir trees
// (including Thunderbird's), directories of .eml files, and mbox files.
// It only reads; deciding what to archive is up to the caller.
package importer

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MaxMessageSize bounds a single imported message.
const MaxMessageSize = 256 << 20

// ErrTooLarge is returned for messages over MaxMessageSize.
var ErrTooLarge = errors.New("message is too large")

// Item is one message found by an importer.
type Item struct {
	Raw     []byte
	Flags   string // Maildir flags (from the filename or Thunderbird headers)
	Folder  string // folder path relative to the import root, "/"-separated
	Source  string // file (and position) it came from, for error messages
	Deleted bool   // marked deleted by the source program; callers skip it
}

// Formats.
const (
	FormatMaildir = "maildir" // Maildir tree and/or directories of .eml files
	FormatMbox    = "mbox"
)

// Detect guesses the format of path: directories are Maildir trees, files
// starting with "From " are mbox.
func Detect(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return FormatMaildir, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	head := make([]byte, 5)
	n, _ := io.ReadFull(f, head)
	if err := f.Close(); err != nil {
		return "", err
	}
	if string(head[:n]) == "From " {
		return FormatMbox, nil
	}
	return "", fmt.Errorf("%s: not a directory or an mbox file", path)
}

// Maildir walks root and yields every message in Maildir folders (files in
// cur/ and new/) and every *.eml file. Symlinks and hidden entries are not
// followed. Errors for individual files are yielded and the walk continues.
func Maildir(root string) iter.Seq2[Item, error] {
	return func(yield func(Item, error) bool) {
		// The root itself may be a symlink (e.g. to an external drive).
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			yield(Item{Source: root}, err)
			return
		}
		root = resolved
		stop := errors.New("stop")
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if !yield(Item{Source: path}, err) {
					return stop
				}
				return nil
			}
			// Hidden entries are skipped, except Maildir++ folders such as
			// ".Sent" or ".INBOX.Archive" (Dovecot, Courier, offlineimap).
			if path != root && strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() && !isMaildirFolder(path) {
					return filepath.SkipDir
				}
				if !d.IsDir() {
					return nil
				}
			}
			if !d.Type().IsRegular() {
				return nil
			}
			item, ok := classify(root, path)
			if !ok {
				return nil
			}
			item.Raw, err = readLimited(path)
			if err == nil {
				item.Flags, item.Deleted = mozillaStatus(item.Raw, item.Flags)
			}
			if !yield(item, err) {
				return stop
			}
			return nil
		})
		if err != nil && !errors.Is(err, stop) {
			yield(Item{Source: root}, err)
		}
	}
}

func isMaildirFolder(dir string) bool {
	for _, sub := range []string{"cur", "new"} {
		if info, err := os.Lstat(filepath.Join(dir, sub)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// classify decides whether path is a message and where it belongs.
func classify(root, path string) (Item, bool) {
	dir, name := filepath.Split(path)
	dir = filepath.Clean(dir)
	parent := filepath.Base(dir)
	inMaildir := isMaildirFolder(filepath.Dir(dir))
	switch {
	case inMaildir && (parent == "cur" || parent == "new"):
		folder := relFolder(root, filepath.Dir(dir))
		return Item{Folder: folder, Source: path, Flags: maildirFlags(name)}, true
	case inMaildir && parent == "tmp":
		return Item{}, false // being written by a mail program
	case strings.EqualFold(filepath.Ext(name), ".eml"):
		return Item{Folder: relFolder(root, dir), Source: path}, true
	default:
		return Item{}, false
	}
}

// relFolder names a folder by its path below the import root. Maildir++
// folder names (".INBOX.Sent") become "INBOX/Sent".
func relFolder(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return filepath.Base(filepath.Clean(root))
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, p := range parts {
		if strings.HasPrefix(p, ".") && len(p) > 1 {
			parts[i] = strings.ReplaceAll(p[1:], ".", "/")
		}
	}
	return strings.Join(parts, "/")
}

// maildirFlags extracts flags from "unique:2,FLAGS" (also ";2," and "!2,",
// which some Windows tools use).
func maildirFlags(name string) string {
	for _, sep := range []string{":2,", ";2,", "!2,"} {
		if i := strings.LastIndex(name, sep); i >= 0 {
			var b strings.Builder
			for _, r := range name[i+3:] {
				if strings.ContainsRune("DFPRST", r) && !strings.ContainsRune(b.String(), r) {
					b.WriteRune(r)
				}
			}
			return b.String()
		}
	}
	return ""
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxMessageSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxMessageSize {
		return nil, ErrTooLarge
	}
	return raw, nil
}

// Thunderbird's X-Mozilla-Status bits.
const (
	mozRead     = 0x0001
	mozReplied  = 0x0002
	mozFlagged  = 0x0004
	mozExpunged = 0x0008
)

// mozillaStatus merges flags from an X-Mozilla-Status header and reports
// whether Thunderbird marked the message deleted. Thunderbird writes that
// header first; anywhere else it may have come from the sender, so it is
// ignored rather than letting a sender hide a message from import.
func mozillaStatus(raw []byte, flags string) (string, bool) {
	value, ok := firstHeader(raw, "X-Mozilla-Status")
	if !ok {
		return flags, isFolderInternalData(raw)
	}
	bits, err := strconv.ParseUint(strings.TrimSpace(value), 16, 32)
	if err != nil {
		return flags, false
	}
	add := func(f string) {
		if !strings.Contains(flags, f) {
			flags += f
		}
	}
	if bits&mozRead != 0 {
		add("S")
	}
	if bits&mozReplied != 0 {
		add("R")
	}
	if bits&mozFlagged != 0 {
		add("F")
	}
	return flags, bits&mozExpunged != 0 || isFolderInternalData(raw)
}

// isFolderInternalData recognizes the pseudo-message some mail programs
// (Pine, older Thunderbird) keep at the start of an mbox file.
func isFolderInternalData(raw []byte) bool {
	subject := headerValue(raw, "Subject")
	return strings.Contains(subject, "FOLDER INTERNAL DATA")
}

// firstHeader returns the value of the message's first header field if it
// is called name.
func firstHeader(raw []byte, name string) (string, bool) {
	line := raw
	if i := bytes.IndexByte(raw, '\n'); i >= 0 {
		line = raw[:i]
	}
	line = bytes.TrimRight(line, "\r")
	prefix := strings.ToLower(name) + ":"
	if len(line) > len(prefix) && strings.EqualFold(string(line[:len(prefix)]), prefix) {
		return string(line[len(prefix):]), true
	}
	return "", false
}

// headerValue finds a top-level header field without parsing the message.
func headerValue(raw []byte, name string) string {
	prefix := []byte(strings.ToLower(name) + ":")
	for len(raw) > 0 {
		line := raw
		if i := bytes.IndexByte(raw, '\n'); i >= 0 {
			line, raw = raw[:i], raw[i+1:]
		} else {
			raw = nil
		}
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			return "" // end of header
		}
		if len(line) > len(prefix) && bytes.Equal(bytes.ToLower(line[:len(prefix)]), prefix) {
			return string(line[len(prefix):])
		}
	}
	return ""
}

// Mbox reads messages from an mbox file. A "From " line opens a new message
// when it looks like an mbox separator ("From <sender> <date>") and is the
// first line or follows an empty line, or when it carries a full date (so a
// writer that omits the blank line does not merge two messages). ">From "
// escaping is undone; note that mboxo files cannot distinguish an escaped
// "From " from a body line that really began with ">From ". The empty line
// before each separator is dropped and line endings are preserved. Memory
// use is bounded even for files without newlines.
func Mbox(r io.Reader, folder, source string) iter.Seq2[Item, error] {
	return func(yield func(Item, error) bool) {
		br := bufio.NewReaderSize(r, 64*1024)
		var msg bytes.Buffer
		started, prevBlank, tooLarge := false, true, false
		lineNo, startLine := 0, 0

		emit := func() bool {
			if !started {
				return true
			}
			src := fmt.Sprintf("%s:%d", source, startLine)
			if tooLarge {
				return yield(Item{Folder: folder, Source: src}, ErrTooLarge)
			}
			raw := trimFinalBlankLine(bytes.Clone(msg.Bytes()))
			flags, deleted := mozillaStatus(raw, "")
			return yield(Item{Raw: raw, Flags: flags, Folder: folder, Source: src, Deleted: deleted}, nil)
		}

		for {
			line, overflow, err := readLine(br, MaxMessageSize)
			if len(line) > 0 || overflow {
				lineNo++
				switch {
				case isSeparator(line, prevBlank || !started):
					if !emit() {
						return
					}
					msg.Reset()
					started, tooLarge, startLine = true, false, lineNo
					prevBlank = false
				case started:
					prevBlank = isBlank(line)
					if !tooLarge {
						msg.Write(unescapeFrom(line))
						tooLarge = overflow || msg.Len() > MaxMessageSize
						if tooLarge {
							msg.Reset()
						}
					}
				case !isBlank(line):
					yield(Item{Folder: folder, Source: source}, errors.New("not an mbox file: no leading \"From \" line"))
					return
				}
			}
			if errors.Is(err, io.EOF) {
				emit()
				return
			}
			if err != nil {
				yield(Item{Folder: folder, Source: source}, err)
				return
			}
		}
	}
}

// readLine reads one line, keeping at most limit bytes of it. overflow
// reports that the line was longer and the rest was discarded.
func readLine(br *bufio.Reader, limit int) (line []byte, overflow bool, err error) {
	for {
		frag, err := br.ReadSlice('\n')
		if len(line)+len(frag) <= limit {
			line = append(line, frag...)
		} else {
			overflow = true
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, overflow, err
		}
	}
}

// mboxDate matches the date part of separators such as
// "From - Tue Oct 14 09:30:00 2026" or "From bob@example.org Tue Oct 14 09:30:00 2026".
var mboxDate = regexp.MustCompile(`^From \S+ +[A-Z][a-z]{2} [A-Z][a-z]{2} +\d{1,2} \d{1,2}:\d{2}(:\d{2})? .*\d{4}`)

func isSeparator(line []byte, afterBlank bool) bool {
	if !bytes.HasPrefix(line, []byte("From ")) {
		return false
	}
	if mboxDate.Match(line) {
		return true
	}
	// Writers that use other date formats: after a blank line, accept
	// "From <sender> <more>" when the sender looks like one, so prose such as
	// "From here on" stays in the body.
	f := bytes.Fields(line)
	if !afterBlank || len(f) < 3 {
		return false
	}
	sender := string(f[1])
	return sender == "-" || sender == "MAILER-DAEMON" || strings.Contains(sender, "@")
}

func isBlank(line []byte) bool {
	return len(bytes.TrimRight(line, "\r\n")) == 0
}

// unescapeFrom turns ">From " into "From " (and ">>From " into ">From ").
func unescapeFrom(line []byte) []byte {
	i := 0
	for i < len(line) && line[i] == '>' {
		i++
	}
	if i > 0 && bytes.HasPrefix(line[i:], []byte("From ")) {
		return line[1:]
	}
	return line
}

// trimFinalBlankLine drops the separator's leading empty line, which mbox
// writers add after every message.
func trimFinalBlankLine(raw []byte) []byte {
	for _, end := range []string{"\r\n\r\n", "\n\n"} {
		if bytes.HasSuffix(raw, []byte(end)) {
			return raw[:len(raw)-len(end)/2]
		}
	}
	return raw
}
