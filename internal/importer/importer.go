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
		stop := errors.New("stop")
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if !yield(Item{Source: path}, err) {
					return stop
				}
				return nil
			}
			if path != root && strings.HasPrefix(d.Name(), ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
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

// classify decides whether path is a message and where it belongs.
func classify(root, path string) (Item, bool) {
	dir, name := filepath.Split(path)
	dir = filepath.Clean(dir)
	parent := filepath.Base(dir)
	switch {
	case parent == "cur" || parent == "new":
		folder := relFolder(root, filepath.Dir(dir))
		return Item{Folder: folder, Source: path, Flags: maildirFlags(name)}, true
	case parent == "tmp":
		return Item{}, false
	case strings.EqualFold(filepath.Ext(name), ".eml"):
		return Item{Folder: relFolder(root, dir), Source: path}, true
	default:
		return Item{}, false
	}
}

func relFolder(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return filepath.Base(filepath.Clean(root))
	}
	return filepath.ToSlash(rel)
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

// mozillaStatus merges flags from an X-Mozilla-Status header, if present,
// and reports whether Thunderbird marked the message deleted.
func mozillaStatus(raw []byte, flags string) (string, bool) {
	value, ok := headerValue(raw, "X-Mozilla-Status")
	if !ok {
		return flags, false
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
	return flags, bits&mozExpunged != 0
}

// headerValue finds a top-level header field without parsing the message.
func headerValue(raw []byte, name string) (string, bool) {
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
			return "", false // end of header
		}
		if len(line) > len(prefix) && bytes.Equal(bytes.ToLower(line[:len(prefix)]), prefix) {
			return string(line[len(prefix):]), true
		}
	}
	return "", false
}

// Mbox reads messages from an mbox file. A line starting with "From " opens
// a new message when it is the first line or follows an empty line. ">From "
// escaping (mboxo and mboxrd) is undone, and the empty line before each
// separator is dropped. Line endings are preserved.
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
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				lineNo++
				switch {
				case bytes.HasPrefix(line, []byte("From ")) && prevBlank:
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
						tooLarge = msg.Len() > MaxMessageSize
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
