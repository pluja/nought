// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pluja/nought/internal/mailmsg"
	"github.com/pluja/nought/internal/render"
)

// SaveIn saves a message's attachments into a directory.
type SaveIn struct {
	ID      string `json:"id"`
	Account string `json:"account,omitempty"`
	Dir     string `json:"dir"`            // created if missing; "~/" is your home
	Only    []int  `json:"only,omitempty"` // attachment numbers, from 1 as listed; none: all
}

// SavedFile is one attachment written to disk.
type SavedFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// MsgSave writes attachments to files. Names come from hostile mail: they
// are reduced to a plain file name, every write stays inside the directory
// (os.Root), and an existing file is never overwritten ("a (1).pdf").
var MsgSave = Op[SaveIn, []SavedFile]{
	Name: "msg.save", Summary: "Save a message's attachments to a directory", Perm: Tag,
	Run: func(ctx context.Context, env *Env, in SaveIn) ([]SavedFile, error) {
		dir := expandPath(in.Dir)
		if dir == "" {
			return nil, usageError("a directory to save into is needed")
		}
		_, raw, _, err := source(ctx, env, in.Account, in.ID)
		if err != nil {
			return nil, err
		}
		atts, err := mailmsg.AttachmentData(raw)
		if err != nil {
			return nil, err
		}
		if len(atts) == 0 {
			return nil, usageError("this message has no attachments")
		}
		chosen := make([]int, 0, len(atts))
		for _, n := range in.Only {
			if n < 1 || n > len(atts) {
				return nil, usageError("no attachment %d: the message has %d", n, len(atts))
			}
			chosen = append(chosen, n-1)
		}
		if len(chosen) == 0 {
			for i := range atts {
				chosen = append(chosen, i)
			}
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return nil, err
		}
		defer func() { _ = root.Close() }()
		out := []SavedFile{}
		for _, i := range chosen {
			name, err := writeNew(root, safeFileName(atts[i].Filename, i+1), atts[i].Data)
			if err != nil {
				return out, fmt.Errorf("save %s: %w", atts[i].Filename, err)
			}
			out = append(out, SavedFile{Name: name, Path: filepath.Join(dir, name), Size: int64(len(atts[i].Data))})
		}
		return out, nil
	},
}

// writeNew writes a file that did not exist: name, else "name (1).ext", ...
func writeNew(root *os.Root, name string, data []byte) (string, error) {
	ext := filepath.Ext(name)
	for n := 0; n < 1000; n++ {
		try := name
		if n > 0 {
			try = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), n, ext)
		}
		f, err := root.OpenFile(try, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		return try, errors.Join(err, f.Sync(), f.Close())
	}
	return "", fmt.Errorf("%s: too many files with that name", name)
}

// safeFileName makes an attachment's name a plain file name that is safe on
// every system: no directories, control or invisible characters, reserved
// characters or Windows device names, and not hidden. The nth attachment
// without a usable name is "attachment-n".
func safeFileName(name string, n int) string {
	name = render.SanitizeLine(name)
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" {
		return fmt.Sprintf("attachment-%d", n)
	}
	switch strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name))) {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		name = "_" + name
	}
	const maxName = 200 // bytes; most file systems allow 255
	if len(name) > maxName {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		stem := strings.ToValidUTF8(name[:maxName-len(ext)], "")
		name = stem + ext
	}
	return name
}
