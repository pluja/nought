// SPDX-License-Identifier: AGPL-3.0-or-later

// Package journal is the append-only record of every message deleted from a
// mail server. An "intent" entry is written and fsynced before the deletion,
// and a "done" entry after it, so the archive's history can always be
// audited: nothing leaves a server without a trace.
package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"nought/internal/atomicfile"
)

// Phases of a deletion.
const (
	Intent = "intent"
	Done   = "done"
)

// Entry records one message's deletion from a server folder.
type Entry struct {
	Time        time.Time `json:"time"`
	Phase       string    `json:"phase"`
	Account     string    `json:"account"`
	Folder      string    `json:"folder"`
	UIDValidity uint32    `json:"uidvalidity"`
	UID         uint32    `json:"uid"`
	MessageID   string    `json:"message_id,omitempty"`
	Hash        string    `json:"hash"` // content hash; names the archived file
}

// Append writes entries as JSON lines and fsyncs before returning.
func Append(path string, entries []Entry) (err error) {
	if len(entries) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create journal directory: %w", err)
	}
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	defer func() {
		err = errors.Join(err, f.Close())
		if err == nil && created {
			// Make the new file's directory entry durable too.
			err = atomicfile.SyncDir(filepath.Dir(path))
		}
	}()
	var buf []byte
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf = append(append(buf, line...), '\n')
	}
	if _, err := f.Write(buf); err != nil {
		return fmt.Errorf("write journal: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync journal: %w", err)
	}
	return nil
}
