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
	"path/filepath"
	"slices"
	"strings"
	"time"
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
	root       *os.Root
	names      map[string]string // hash -> path relative to root, e.g. "cur/<hash>:2,S"
	foreign    int
	duplicates int
	hooks      hooks
	now        func() time.Time
}

// staleTemp is how old a temp file must be before Open removes it, so a
// concurrent writer's in-flight file is never touched (Maildir convention).
const staleTemp = 36 * time.Hour

// Hash returns the content address of a raw message.
func Hash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Open opens (creating if needed) the Maildir at dir, indexes its files, and
// removes temporary files left behind by an interrupted write.
func Open(dir string) (*Maildir, error) {
	for _, sub := range []string{"", "tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("create maildir: %w", err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open maildir: %w", err)
	}
	m := &Maildir{root: root, now: time.Now}
	if err := m.scan(); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	if err := m.removeStaleTemp(); err != nil {
		return nil, errors.Join(err, root.Close())
	}
	return m, nil
}

// Close releases the directory handle.
func (m *Maildir) Close() error { return m.root.Close() }

// Foreign is the number of files in cur/ and new/ that nought did not write.
// They are left alone.
func (m *Maildir) Foreign() int { return m.foreign }

// Duplicates is the number of extra files holding an already seen hash
// (e.g. the same message with different flags, left by another program).
// The lexicographically smallest name wins; the others are left alone.
func (m *Maildir) Duplicates() int { return m.duplicates }

// Len is the number of stored messages.
func (m *Maildir) Len() int { return len(m.names) }

// Has reports whether a message is stored.
func (m *Maildir) Has(hash string) bool {
	_, ok := m.names[hash]
	return ok
}

// scan rebuilds the hash -> name map from cur/ and new/.
func (m *Maildir) scan() error {
	names := make(map[string]string)
	foreign, duplicates := 0, 0
	for _, sub := range []string{"new", "cur"} {
		entries, err := m.readDir(sub)
		if err != nil {
			return err
		}
		for _, e := range entries {
			hash, _, ok := parseName(e.Name())
			if !ok || !e.Type().IsRegular() {
				foreign++
				continue
			}
			name := sub + "/" + e.Name()
			if prev, seen := names[hash]; seen {
				duplicates++
				if prev < name {
					continue
				}
			}
			names[hash] = name
		}
	}
	m.names, m.foreign, m.duplicates = names, foreign, duplicates
	return nil
}

func (m *Maildir) removeStaleTemp() error {
	entries, err := m.readDir("tmp")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !isOurTemp(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || m.now().Sub(info.ModTime()) < staleTemp {
			continue
		}
		if err := m.root.Remove("tmp/" + e.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove stale temp file: %w", err)
		}
	}
	return nil
}

// lookup returns the current name of hash, rescanning once if the file was
// renamed by another program since the last scan.
func (m *Maildir) lookup(hash string) (string, error) {
	if name, ok := m.names[hash]; ok {
		if _, err := m.root.Lstat(name); err == nil {
			return name, nil
		}
	}
	if err := m.scan(); err != nil {
		return "", err
	}
	if name, ok := m.names[hash]; ok {
		return name, nil
	}
	return "", ErrNotFound
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
// newly written. A message already stored intact keeps its current flags. If
// the stored copy is damaged, it is replaced with raw. Put only succeeds once
// the file on disk has been read back and matches the hash.
func (m *Maildir) Put(raw []byte, flags string) (Entry, bool, error) {
	hash := Hash(raw)
	flags, err := NormalizeFlags(flags)
	if err != nil {
		return Entry{}, false, err
	}

	final := "cur/" + fileName(hash, flags)
	// Only hashes already known are looked up again; rescanning the
	// directory for every new message would make imports quadratic.
	if _, known := m.names[hash]; !known {
		return m.create(raw, hash, flags, final)
	}
	if name, err := m.lookup(hash); err == nil {
		verr := m.verifyName(name, hash)
		if verr == nil {
			_, existing, _ := parseName(path.Base(name))
			return Entry{Hash: hash, Flags: existing}, false, nil
		}
		if !errors.Is(verr, ErrCorrupt) && !errors.Is(verr, ErrNotFound) {
			return Entry{}, false, verr
		}
		// Damaged copy: overwrite it in place, keeping its flags.
		final = name
		_, flags, _ = parseName(path.Base(name))
	} else if !errors.Is(err, ErrNotFound) {
		return Entry{}, false, err
	}

	return m.create(raw, hash, flags, final)
}

// create writes raw to final and registers it only once verified.
func (m *Maildir) create(raw []byte, hash, flags, final string) (Entry, bool, error) {
	delete(m.names, hash)
	if err := m.write(raw, hash, final); err != nil {
		return Entry{}, false, err
	}
	if err := m.verifyName(final, hash); err != nil {
		return Entry{}, false, err
	}
	m.names[hash] = final
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
	name, err := m.lookup(hash)
	if err != nil {
		return nil, err
	}
	return m.readName(name)
}

func (m *Maildir) readName(name string) ([]byte, error) {
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
	name, err := m.lookup(hash)
	if err != nil {
		return err
	}
	return m.verifyName(name, hash)
}

func (m *Maildir) verifyName(name, hash string) error {
	raw, err := m.readName(name)
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
	name, err := m.lookup(hash)
	if err != nil {
		return "", err
	}
	_, flags, _ := parseName(path.Base(name))
	return flags, nil
}

// SetFlags renames a message to carry new flags. The content never changes.
func (m *Maildir) SetFlags(hash, flags string) error {
	flags, err := NormalizeFlags(flags)
	if err != nil {
		return err
	}
	name, err := m.lookup(hash)
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
