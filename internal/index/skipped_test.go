// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"slices"
	"testing"
)

func skippedUIDs(t *testing.T, ix *Index, folder string, validity uint32) []uint32 {
	t.Helper()
	got, err := ix.Skipped(t.Context(), "a", folder, validity)
	if err != nil {
		t.Fatal(err)
	}
	var uids []uint32
	for _, s := range got {
		uids = append(uids, s.UID)
	}
	return uids
}

func TestSkippedMessages(t *testing.T) {
	ix, _ := openIndex(t)
	ctx := t.Context()
	for _, s := range []struct {
		folder string
		uid    uint32
	}{{"INBOX", 7}, {"INBOX", 5}, {"INBOX", 5}, {"Sent", 1}} {
		if err := ix.RecordSkipped(ctx, "a", s.folder, 1, s.uid, 300<<20); err != nil {
			t.Fatal(err)
		}
	}
	if got := skippedUIDs(t, ix, "INBOX", 1); !slices.Equal(got, []uint32{5, 7}) {
		t.Errorf("skipped = %v", got)
	}
	if err := ix.ForgetSkipped(ctx, "a", "INBOX", 1, 5); err != nil {
		t.Fatal(err)
	}
	if got := skippedUIDs(t, ix, "INBOX", 1); !slices.Equal(got, []uint32{7}) {
		t.Errorf("after forgetting 5 = %v", got)
	}
	// A new UIDVALIDITY makes the old UIDs meaningless.
	if err := ix.ResetFolder(ctx, "a", "INBOX", 2); err != nil {
		t.Fatal(err)
	}
	if got := skippedUIDs(t, ix, "INBOX", 1); len(got) != 0 {
		t.Errorf("kept after a reset = %v", got)
	}
	if err := ix.ForgetFolder(ctx, "a", "Sent", 1); err != nil {
		t.Fatal(err)
	}
	if got := skippedUIDs(t, ix, "Sent", 1); len(got) != 0 {
		t.Errorf("kept after forgetting the folder = %v", got)
	}
}
