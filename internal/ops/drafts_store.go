// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pluja/nought/internal/atomicfile"
	"github.com/pluja/nought/internal/compose"
)

// Drafts on disk: an editor file (.txt) and its details (.json), under
// random ids that say nothing about the message.

// listDrafts lists the drafts, most recently changed first.
func listDrafts(env *Env) ([]DraftInfo, error) {
	entries, err := os.ReadDir(env.Paths.DraftsDir)
	if errors.Is(err, os.ErrNotExist) {
		return []DraftInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []DraftInfo{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !validID(id) {
			continue
		}
		info, err := loadDraftInfo(env, id)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(x, y DraftInfo) int { return y.Updated.Compare(x.Updated) })
	return out, nil
}

// resumeReply finds the most recently changed reply (or reply-all) to a
// message that is still a draft, not on its way in the outbox.
func resumeReply(ctx context.Context, env *Env, account, hash string) (DraftOut, bool, error) {
	drafts, err := listDrafts(env)
	if err != nil {
		return DraftOut{}, false, err
	}
	for _, info := range drafts {
		if info.Account != account || info.SourceHash != hash || info.Kind != KindReply && info.Kind != KindReplyAll {
			continue
		}
		_, sending, err := queued(env, info.ID)
		if err != nil {
			return DraftOut{}, false, err
		}
		if sending {
			continue
		}
		info, d, err := loadDraft(env, info.ID)
		if err != nil {
			return DraftOut{}, false, err
		}
		out := draftOut(ctx, env, info, d)
		out.Resumed = true
		return out, true, nil
	}
	return DraftOut{}, false, nil
}

// createDraft saves a new draft. given: the caller supplied some of its
// content, so it is never dropped as untouched.
func createDraft(ctx context.Context, env *Env, info DraftInfo, d compose.Draft, given bool) (DraftOut, error) {
	id, err := newID()
	if err != nil {
		return DraftOut{}, err
	}
	now := env.Now().UTC()
	info.ID, info.Created, info.Updated = id, now, now
	info.Subject, info.To, info.InReplyTo, info.References = d.Subject, d.To, d.InReplyTo, d.References
	info.Path = draftPath(env, id, ".txt")
	if !given {
		info.Untouched = fileHash(compose.Render(d))
	}
	if err := writeDraftFile(info.Path, d); err != nil {
		return DraftOut{}, err
	}
	if err := saveDraftInfo(env, info); err != nil {
		return DraftOut{}, err
	}
	return draftOut(ctx, env, info, d), nil
}

func fileHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func writeDraftFile(path string, d compose.Draft) error {
	if err := atomicfile.Write(path, 0o600, func(w io.Writer) error {
		_, err := io.WriteString(w, compose.Render(d))
		return err
	}); err != nil {
		return fmt.Errorf("write draft: %w", err)
	}
	return nil
}

func saveDraftInfo(env *Env, info DraftInfo) error {
	return atomicfile.Write(draftPath(env, info.ID, ".json"), 0o600, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(info)
	})
}

func loadDraftInfo(env *Env, id string) (DraftInfo, error) {
	if !validID(id) {
		return DraftInfo{}, usageError("%q is not a draft id", id)
	}
	b, err := os.ReadFile(draftPath(env, id, ".json"))
	if errors.Is(err, os.ErrNotExist) {
		return DraftInfo{}, fmt.Errorf("draft %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return DraftInfo{}, err
	}
	var info DraftInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return DraftInfo{}, fmt.Errorf("draft %s: %w", id, err)
	}
	info.ID, info.Path = id, draftPath(env, id, ".txt") // never trust a path from disk
	return info, nil
}

// loadDraft reads a draft's details and its editor file.
func loadDraft(env *Env, id string) (DraftInfo, compose.Draft, error) {
	info, err := loadDraftInfo(env, id)
	if err != nil {
		return info, compose.Draft{}, err
	}
	text, err := os.ReadFile(info.Path)
	if err != nil {
		return info, compose.Draft{}, fmt.Errorf("draft %s: %w", id, err)
	}
	d, err := compose.Parse(string(text), compose.Draft{InReplyTo: info.InReplyTo, References: info.References})
	if err != nil {
		return info, d, usageError("draft %s: %v", id, err)
	}
	return info, d, nil
}

// removeDraft removes the metadata first: a crash in between leaves an
// unlisted file, never a draft that cannot be opened.
func removeDraft(env *Env, id string) error {
	var errs []error
	for _, ext := range []string{".json", ".txt"} {
		if err := os.Remove(draftPath(env, id, ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func draftPath(env *Env, id, ext string) string { return filepath.Join(env.Paths.DraftsDir, id+ext) }

// newID is a random id: nothing about the message is in file names.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// validID accepts only ids newID makes, so an id can never leave the
// drafts directory.
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}
