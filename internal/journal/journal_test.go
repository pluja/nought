// SPDX-License-Identifier: AGPL-3.0-or-later

package journal

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "expunge.journal")
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	first := []Entry{{Time: at, Phase: Intent, Account: "a", Folder: "INBOX", UIDValidity: 7, UID: 1, Hash: "h1"}}
	second := []Entry{{Time: at, Phase: Done, Account: "a", Folder: "INBOX", UIDValidity: 7, UID: 1, Hash: "h1"}}
	for _, batch := range [][]Entry{first, nil, second} {
		if err := Append(path, batch); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var got []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if len(got) != 2 || got[0] != first[0] || got[1] != second[0] {
		t.Errorf("journal = %+v", got)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o600 {
			t.Errorf("perm = %o", info.Mode().Perm())
		}
	}
}

func TestAppendUnwritable(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, []Entry{{Phase: Intent}}); err == nil {
		t.Fatal("appended to a directory")
	}
}
