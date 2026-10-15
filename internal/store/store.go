// SPDX-License-Identifier: AGPL-3.0-or-later

// Package store keeps raw messages as immutable files in a Maildir.
//
// Files are named after the SHA-256 of their content, so writing the same
// message twice is a no-op and backups deduplicate perfectly. Only the flag
// suffix of a filename ever changes. Every write is verified: the file is
// fsynced, renamed into cur/, the directory is fsynced, and the file is read
// back and hashed before Put reports success.
//
// A Maildir is not safe for concurrent use.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"path"
	"slices"
	"strings"
)

// Errors returned by the store.
var (
	ErrNotFound = errors.New("message not in store")
	ErrCorrupt  = errors.New("message file does not match its hash")
)

// Entry identifies a stored message.
type Entry struct {
	Hash  string
	Flags string
}

// hooks let tests simulate crashes between the steps of a write.
type hooks struct {
	afterWrite, afterSync, afterRename func() error
}

// Maildir is one account's archive directory.
type Maildir struct {
	root    *os.Root
	names   map[string]string // hash -> path relative to root, e.g. "cur/<hash>:2,S"
	foreign int
	hooks   hooks
}

// Hash returns the content address of a raw message.
func Hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Open opens (creating if needed) the Maildir at dir, indexes its files, and
// removes temporary files left behind by an interrupted write.
func Open(dir string) (*Maildir, error) {
	for _, sub := range []string{"", "tmp", "new", "cur"} {
		if err := os.MkdirAll(path.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("create maildir: %w", err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open maildir: %w", err)
	}
	m := &Maildir{root: root, names: make(map[string]string)}
	if err := m.scan(); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return m, nil
}

// Close releases the directory handle.
func (m *Maildir) Close() error { return m.root.Close() }

// Foreign is the number of files in cur/ and new/ that nought did not write.
// They are left alone.
func (m *Maildir) Foreign() int { return m.foreign }

// Len is the number of stored messages.
func (m *Maildir) Len() int { return len(m.names) }

// Has reports whether a message is stored.
func (m *Maildir) Has(hash string) bool {
	_, ok := m.names[hash]
	return ok
}

func (m *Maildir) scan() error {
	for _, sub := range []string{"new", "cur"} {
		entries, err := m.readDir(sub)
		if err != nil {
			return err
		}
		for _, e := range entries {
			hash, _, ok := parseName(e.Name())
			if !ok || !e.Type().IsRegular() {
				m.foreign++
				continue
			}
			m.names[hash] = sub + "/" + e.Name()
		}
	}
	tmp, err := m.readDir("tmp")
	if err != nil {
		return err
	}
	for _, e := range tmp {
		if isOurTemp(e.Name()) {
			if err := m.root.Remove("tmp/" + e.Name()); err != nil {
				return fmt.Errorf("remove stale temp file: %w", err)
			}
		}
	}
	return nil
}

func (m *Maildir) readDir(sub string) ([]fs.DirEntry, error) {
	d, err := m.root.Open(sub)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", sub, err)
	}
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("read %s: %w", sub, err), d.Close())
	}
	return entries, d.Close()
}

// Put stores raw with the given Maildir flags and reports whether it was
// newly created. An already stored message keeps its current flags.
func (m *Maildir) Put(raw []byte, flags string) (Entry, bool, error) {
	hash := Hash(raw)
	if name, ok := m.names[hash]; ok {
		_, existing, _ := parseName(path.Base(name))
		return Entry{Hash: hash, Flags: existing}, false, nil
	}
	flags, err := NormalizeFlags(flags)
	if err != nil {
		return Entry{}, false, err
	}

	final := "cur/" + fileName(hash, flags)
	if err := m.write(raw, hash, final); err != nil {
		return Entry{}, false, err
	}
	m.names[hash] = final
	if err := m.Verify(hash); err != nil {
		return Entry{}, false, err
	}
	return Entry{Hash: hash, Flags: flags}, true, nil
}

func (m *Maildir) write(raw []byte, hash, final string) (err error) {
	tmp := "tmp/" + tempName(hash)
	f, err := m.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	renamed := false
	defer func() {
		if err != nil && !renamed {
			err = errors.Join(err, m.root.Remove(tmp))
		}
	}()

	if _, err := f.Write(raw); err != nil {
		return errors.Join(fmt.Errorf("write message: %w", err), f.Close())
	}
	if err := runHook(m.hooks.afterWrite); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync message: %w", err), f.Close())
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close message: %w", err)
	}
	if err := runHook(m.hooks.afterSync); err != nil {
		return err
	}
	if err := m.root.Rename(tmp, final); err != nil {
		return fmt.Errorf("move message into cur: %w", err)
	}
	renamed = true
	if err := runHook(m.hooks.afterRename); err != nil {
		return err
	}
	return m.syncDir("cur")
}

func runHook(h func() error) error {
	if h == nil {
		return nil
	}
	return h()
}

func (m *Maildir) syncDir(sub string) error {
	d, err := m.root.Open(sub)
	if err != nil {
		return fmt.Errorf("open %s: %w", sub, err)
	}
	if err := syncDir(d); err != nil {
		return errors.Join(fmt.Errorf("sync %s: %w", sub, err), d.Close())
	}
	return d.Close()
}

// Get returns the raw bytes of a stored message.
func (m *Maildir) Get(hash string) ([]byte, error) {
	name, ok := m.names[hash]
	if !ok {
		return nil, ErrNotFound
	}
	raw, err := m.root.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read message: %w", err)
	}
	return raw, nil
}

// Verify re-reads a stored message and checks it still matches its hash.
func (m *Maildir) Verify(hash string) error {
	raw, err := m.Get(hash)
	if err != nil {
		return err
	}
	if Hash(raw) != hash {
		return fmt.Errorf("%s: %w", hash[:12], ErrCorrupt)
	}
	return nil
}

// Flags returns the current Maildir flags of a message.
func (m *Maildir) Flags(hash string) (string, error) {
	name, ok := m.names[hash]
	if !ok {
		return "", ErrNotFound
	}
	_, flags, _ := parseName(path.Base(name))
	return flags, nil
}

// SetFlags renames a message to carry new flags. The content never changes.
func (m *Maildir) SetFlags(hash, flags string) error {
	name, ok := m.names[hash]
	if !ok {
		return ErrNotFound
	}
	flags, err := NormalizeFlags(flags)
	if err != nil {
		return err
	}
	dir := path.Dir(name)
	next := dir + "/" + fileName(hash, flags)
	if next == name {
		return nil
	}
	if err := m.root.Rename(name, next); err != nil {
		return fmt.Errorf("rename message: %w", err)
	}
	m.names[hash] = next
	return m.syncDir(dir)
}

// All yields every stored message in hash order.
func (m *Maildir) All() iter.Seq[Entry] {
	return func(yield func(Entry) bool) {
		hashes := make([]string, 0, len(m.names))
		for h := range m.names {
			hashes = append(hashes, h)
		}
		slices.Sort(hashes)
		for _, h := range hashes {
			_, flags, _ := parseName(path.Base(m.names[h]))
			if !yield(Entry{Hash: h, Flags: flags}) {
				return
			}
		}
	}
}

// validFlags are the standard Maildir flags, in the required ASCII order.
const validFlags = "DFPRST"

// NormalizeFlags sorts and deduplicates Maildir flags, rejecting unknown ones.
func NormalizeFlags(flags string) (string, error) {
	var b strings.Builder
	for _, f := range validFlags {
		if strings.ContainsRune(flags, f) {
			b.WriteRune(f)
		}
	}
	for _, f := range flags {
		if !strings.ContainsRune(validFlags, f) {
			return "", fmt.Errorf("invalid maildir flag %q", f)
		}
	}
	return b.String(), nil
}

func fileName(hash, flags string) string {
	return hash + infoSep + "2," + flags
}

// parseName splits "<hash>:2,<flags>" (or ";" on Windows). Both separators
// are accepted so an archive can move between systems.
func parseName(name string) (hash, flags string, ok bool) {
	if len(name) < 64+3 {
		return "", "", false
	}
	hash, info := name[:64], name[64:]
	if !isHex(hash) || (info[0] != ':' && info[0] != ';') || info[1:3] != "2," {
		return "", "", false
	}
	flags, err := NormalizeFlags(info[3:])
	if err != nil {
		return "", "", false
	}
	return hash, flags, true
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

const tempPrefix = "nought-"

func tempName(hash string) string {
	return tempPrefix + hash[:16] + "-" + rand.Text()
}

func isOurTemp(name string) bool { return strings.HasPrefix(name, tempPrefix) }
