// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"nought/internal/query"
)

var now = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func openIndex(t *testing.T) (*Index, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "index.db")
	ix, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix, path
}

func hash(n int) string { return strings.Repeat(string("0123456789abcdef"[n%16]), 64) }

func add(t *testing.T, ix *Index, m Message, body string) int64 {
	t.Helper()
	id, created, err := ix.AddMessage(t.Context(), m, body, now)
	if err != nil || !created {
		t.Fatalf("AddMessage(%s) = %v, %v", m.Subject, created, err)
	}
	return id
}

func sample(t *testing.T, ix *Index) {
	t.Helper()
	add(t, ix, Message{Account: "personal", Hash: hash(1), MessageID: "one@x", Subject: "Quarterly report",
		From: "Alice <alice@example.org>", To: []string{"me@example.org"}, Size: 10,
		Date: time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), Tags: []string{"inbox", "unread"}}, "numbers are up")
	add(t, ix, Message{Account: "personal", Hash: hash(2), MessageID: "two@x", Subject: "Lunch?",
		From: "Bob <bob@example.org>", To: []string{"me@example.org", "Carol <carol@example.org>"}, Size: 20,
		Date: time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC), Tags: []string{"inbox"}}, "pizza on friday")
	add(t, ix, Message{Account: "work", Hash: hash(3), MessageID: "three@x", Subject: "Café meeting",
		From: "boss@corp.example", Size: 30,
		Date: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), Tags: []string{"folder:sent"}}, "agenda attached")
	add(t, ix, Message{Account: "work", Hash: hash(4), Subject: "no date, no id", Size: 1}, "")
}

func TestOpenCreatesPrivateFileAndMigrates(t *testing.T) {
	ix, path := openIndex(t)
	var version int
	if err := ix.db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil || version != 4 {
		t.Fatalf("user_version = %d, %v", version, err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("perm = %o", info.Mode().Perm())
		}
	}
	// Reopening is a no-op migration.
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	_ = again.Close()
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	ix, path := openIndex(t)
	if _, err := ix.db.ExecContext(t.Context(), "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	_ = ix.Close()
	if _, err := Open(t.Context(), path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenRejectsDSNCharacters(t *testing.T) {
	for _, name := range []string{"a?b.db", "a#b.db", "a%41b.db"} {
		if _, err := Open(t.Context(), filepath.Join(t.TempDir(), name)); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
}

func TestAddMessageIsIdempotent(t *testing.T) {
	ix, _ := openIndex(t)
	m := Message{Account: "personal", Hash: hash(1), Subject: "x", Tags: []string{"inbox"}}
	id := add(t, ix, m, "")
	again, created, err := ix.AddMessage(t.Context(), Message{Account: "personal", Hash: hash(1), Tags: []string{"other"}}, "", now)
	if err != nil || created || again != id {
		t.Fatalf("second add = %d, %v, %v", again, created, err)
	}
	got, err := ix.Get(t.Context(), id)
	if err != nil || !slices.Equal(got.Tags, []string{"inbox"}) {
		t.Errorf("tags after duplicate add = %v, %v", got.Tags, err)
	}
	// Same hash in another account is a separate message.
	add(t, ix, Message{Account: "work", Hash: hash(1)}, "")
}

func TestAddMessageValidates(t *testing.T) {
	ix, _ := openIndex(t)
	if _, _, err := ix.AddMessage(t.Context(), Message{Account: "a", Hash: hash(1), Tags: []string{"Bad Tag"}}, "", now); err == nil {
		t.Error("accepted an invalid tag")
	}
	if _, _, err := ix.AddMessage(t.Context(), Message{Hash: hash(1)}, "", now); err == nil {
		t.Error("accepted a message without account")
	}
}

func TestGetRoundTrip(t *testing.T) {
	ix, _ := openIndex(t)
	sample(t, ix)
	m, err := ix.Resolve(t.Context(), "", hash(2)[:8])
	if err != nil {
		t.Fatal(err)
	}
	if m.Account != "personal" || m.MessageID != "two@x" || m.Subject != "Lunch?" || m.Size != 20 ||
		len(m.To) != 2 || !m.Date.Equal(time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("message = %+v", m)
	}
	if _, err := ix.Get(t.Context(), 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing = %v", err)
	}
}

func TestResolve(t *testing.T) {
	ix, _ := openIndex(t)
	sample(t, ix)
	add(t, ix, Message{Account: "work", Hash: hash(1)}, "") // same hash, other account

	if _, err := ix.Resolve(t.Context(), "", hash(1)[:6]); err == nil {
		t.Fatal("expected ambiguity")
	} else if amb, ok := errors.AsType[*AmbiguousError](err); !ok || len(amb.Candidates) != 2 ||
		!strings.Contains(err.Error(), "matches 2 messages") {
		t.Fatalf("err = %v", err)
	}
	m, err := ix.Resolve(t.Context(), "work", strings.ToUpper(hash(1)[:6]))
	if err != nil || m.Account != "work" {
		t.Fatalf("Resolve with account = %+v, %v", m, err)
	}
	if _, err := ix.Resolve(t.Context(), "", "abcdef"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing = %v", err)
	}
	for _, bad := range []string{"abc", "zzzzzz", strings.Repeat("a", 65)} {
		if _, err := ix.Resolve(t.Context(), "", bad); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Resolve(%q) = %v, want a validation error", bad, err)
		}
	}
}

func TestHasHashAndMessageID(t *testing.T) {
	ix, _ := openIndex(t)
	sample(t, ix)
	ctx := t.Context()
	for _, tt := range []struct {
		fn   func(context.Context, string, string) (bool, error)
		acct string
		arg  string
		want bool
	}{
		{ix.HasHash, "personal", hash(1), true},
		{ix.HasHash, "work", hash(1), false},
		{ix.HasMessageID, "personal", "two@x", true},
		{ix.HasMessageID, "work", "two@x", false},
		{ix.HasMessageID, "work", "", false},
	} {
		got, err := tt.fn(ctx, tt.acct, tt.arg)
		if err != nil || got != tt.want {
			t.Errorf("(%s, %s) = %v, %v; want %v", tt.acct, tt.arg, got, err, tt.want)
		}
	}
}

func TestSearch(t *testing.T) {
	ix, _ := openIndex(t)
	sample(t, ix)
	tests := []struct {
		q    string
		want []string // subjects, newest first
	}{
		{"", []string{"Café meeting", "Lunch?", "Quarterly report", "no date, no id"}},
		{"tag:inbox", []string{"Lunch?", "Quarterly report"}},
		{"is:unread", []string{"Quarterly report"}},
		{"-is:unread tag:inbox", []string{"Lunch?"}},
		{"tag:inbox OR account:work", []string{"Café meeting", "Lunch?", "Quarterly report", "no date, no id"}},
		{"from:ALICE", []string{"Quarterly report"}},
		{"to:carol", []string{"Lunch?"}},
		{"subject:lunch", []string{"Lunch?"}},
		{"account:work", []string{"Café meeting", "no date, no id"}},
		{"id:three@x", []string{"Café meeting"}},
		{"pizza", []string{"Lunch?"}},
		{"cafe", []string{"Café meeting"}}, // diacritics folded
		{`"numbers are"`, []string{"Quarterly report"}},
		{`"numbers up"`, nil}, // phrase, not words
		{`"NEAR(x y)"`, nil},  // FTS operators are literal
		{`"subject:report"`, nil},
		{"date:2026-03-01..2026-12-31", []string{"Café meeting", "Lunch?"}},
		{"date:..2026-01-10", []string{"Quarterly report"}},
		{"-date:2026-01-01..", []string{"no date, no id"}},
		{"tag:folder:sent", []string{"Café meeting"}},
		{"hash:" + hash(2)[:8], []string{"Lunch?"}},
		{"hash:" + hash(2)[:8] + " OR hash:" + hash(3)[:6], []string{"Café meeting", "Lunch?"}},
	}
	for _, tt := range tests {
		q, err := query.Parse(tt.q)
		if err != nil {
			t.Fatalf("parse %q: %v", tt.q, err)
		}
		msgs, err := ix.Search(t.Context(), q, 0, 0)
		if err != nil {
			t.Fatalf("Search(%q): %v", tt.q, err)
		}
		var got []string
		for _, m := range msgs {
			got = append(got, m.Subject)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("Search(%q) = %q, want %q", tt.q, got, tt.want)
		}
	}
}

func TestCount(t *testing.T) {
	ix, _ := openIndex(t)
	sample(t, ix)
	for q, want := range map[string]int{"": 4, "tag:inbox": 2, "account:work": 2, "tag:nope": 0} {
		parsed, _ := query.Parse(q)
		if n, err := ix.Count(t.Context(), parsed); err != nil || n != want {
			t.Errorf("Count(%q) = %d, %v; want %d", q, n, err, want)
		}
	}
}

func TestSearchPaging(t *testing.T) {
	ix, _ := openIndex(t)
	sample(t, ix)
	page, err := ix.Search(t.Context(), query.Query{}, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].Subject != "Lunch?" || page[1].Subject != "Quarterly report" {
		t.Errorf("page = %+v", page)
	}
	if _, err := ix.Search(t.Context(), query.Query{}, -1, -5); err != nil {
		t.Error(err)
	}
}

func TestModifyTagsAndCounts(t *testing.T) {
	ix, _ := openIndex(t)
	sample(t, ix)
	m, _ := ix.Resolve(t.Context(), "", hash(1)[:6])
	if err := ix.ModifyTags(t.Context(), []int64{m.ID}, []string{"todo", "todo"}, []string{"inbox"}); err != nil {
		t.Fatal(err)
	}
	m, _ = ix.Get(t.Context(), m.ID)
	if !slices.Equal(m.Tags, []string{"todo", "unread"}) {
		t.Errorf("tags = %v", m.Tags)
	}
	if err := ix.ModifyTags(t.Context(), []int64{m.ID}, []string{"UPPER"}, nil); err == nil {
		t.Error("accepted uppercase tag")
	}
	counts, err := ix.TagCounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []TagCount{{"folder:sent", 1}, {"inbox", 1}, {"todo", 1}, {"unread", 1}}
	if !slices.Equal(counts, want) {
		t.Errorf("counts = %v, want %v", counts, want)
	}
}

func TestValidateTag(t *testing.T) {
	for _, ok := range []string{"inbox", "folder:sent-items", "ai/newsletter", "café"} {
		if err := ValidateTag(ok); err != nil {
			t.Errorf("ValidateTag(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "Inbox", "x\x00", "+x", "-x", strings.Repeat("a", MaxTagLen+1)} {
		if err := ValidateTag(bad); err == nil {
			t.Errorf("ValidateTag(%q) accepted", bad)
		}
	}
}

func TestDumpRestoreRoundTrip(t *testing.T) {
	ix, _ := openIndex(t)
	sample(t, ix)
	// Duplicate Message-ID within an account forces hash keys.
	add(t, ix, Message{Account: "personal", Hash: hash(5), MessageID: "one@x", Tags: []string{"dup"}}, "")
	m, _ := ix.Resolve(t.Context(), "", hash(2)[:6])
	if err := ix.ModifyTags(t.Context(), []int64{m.ID}, []string{"weird%tag"}, nil); err != nil {
		t.Fatal(err)
	}

	var dump bytes.Buffer
	if err := ix.Dump(t.Context(), &dump, nil); err != nil {
		t.Fatal(err)
	}
	text := dump.String()
	for _, want := range []string{
		"# account personal\n",
		"+inbox +weird%25tag -- id:two@x\n",
		"-- hash:" + hash(1) + "\n",
		"# account work\n",
		"-- hash:" + hash(4) + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("dump lacks %q:\n%s", want, text)
		}
	}

	before := allTags(t, ix)
	fresh, _ := openIndex(t)
	sampleNoTags(t, fresh)
	add(t, fresh, Message{Account: "personal", Hash: hash(5), MessageID: "one@x"}, "")
	res, err := fresh.Restore(t.Context(), strings.NewReader(text+"-- id:missing@x\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 5 || res.Missing != 1 {
		t.Errorf("restore = %+v", res)
	}
	if after := allTags(t, fresh); !slices.Equal(before, after) {
		t.Errorf("tags differ after restore:\nbefore %v\nafter  %v", before, after)
	}
}

func sampleNoTags(t *testing.T, ix *Index) {
	t.Helper()
	for i, acct := range []string{"personal", "personal", "work", "work"} {
		ids := []string{"one@x", "two@x", "three@x", ""}
		add(t, ix, Message{Account: acct, Hash: hash(i + 1), MessageID: ids[i]}, "")
	}
}

func allTags(t *testing.T, ix *Index) []string {
	t.Helper()
	msgs, err := ix.Search(t.Context(), query.Query{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range msgs {
		out = append(out, m.Account+"/"+m.Hash[:4]+"="+strings.Join(m.Tags, ","))
	}
	slices.Sort(out)
	return out
}

func TestDumpRestoreTricky(t *testing.T) {
	ix, _ := openIndex(t)
	longID := strings.Repeat(`"`, 2000) + "@x"
	add(t, ix, Message{Account: "a", Hash: hash(1), MessageID: "ok@x", Tags: []string{"a--", "b--c", "x%y", "z-"}}, "")
	add(t, ix, Message{Account: "a", Hash: hash(2), MessageID: longID, Tags: []string{"long"}}, "")
	add(t, ix, Message{Account: "a", Hash: hash(3), MessageID: "has space@x", Tags: []string{"sp"}}, "")
	var dump bytes.Buffer
	if err := ix.Dump(t.Context(), &dump, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump.String(), longID[:100]) {
		t.Error("long Message-ID written as id: key")
	}
	before := allTags(t, ix)

	fresh, _ := openIndex(t)
	add(t, fresh, Message{Account: "a", Hash: hash(1), MessageID: "ok@x"}, "")
	add(t, fresh, Message{Account: "a", Hash: hash(2), MessageID: longID}, "")
	add(t, fresh, Message{Account: "a", Hash: hash(3), MessageID: "has space@x"}, "")
	res, err := fresh.Restore(t.Context(), &dump)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if res.Applied != 3 || res.Missing != 0 {
		t.Errorf("restore = %+v", res)
	}
	if after := allTags(t, fresh); !slices.Equal(before, after) {
		t.Errorf("tags differ:\n%v\n%v", before, after)
	}
}

func TestRestoreSkipsUnrepresentableTags(t *testing.T) {
	ix, _ := openIndex(t)
	add(t, ix, Message{Account: "a", Hash: hash(1), MessageID: "x@y"}, "")
	// A notmuch dump with an uppercase tag and a tag containing a space.
	res, err := ix.Restore(t.Context(), strings.NewReader("+Inbox +with%20space -- id:x@y\n"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != 1 || res.SkippedTags != 1 {
		t.Errorf("restore = %+v", res)
	}
	m, _ := ix.Resolve(t.Context(), "", hash(1)[:6])
	if !slices.Equal(m.Tags, []string{"inbox"}) {
		t.Errorf("tags = %v", m.Tags)
	}
}

func TestConcurrentFirstOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			ix, err := Open(context.Background(), path)
			if err == nil {
				err = ix.Close()
			}
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent open: %v", err)
		}
	}
}

func TestAddMessageRejectsBadHash(t *testing.T) {
	ix, _ := openIndex(t)
	for _, h := range []string{"short", strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if _, _, err := ix.AddMessage(t.Context(), Message{Account: "a", Hash: h}, "", now); err == nil {
			t.Errorf("accepted hash %q", h)
		}
	}
}

func TestRestoreErrors(t *testing.T) {
	ix, _ := openIndex(t)
	for _, line := range []string{
		"+inbox id:x",
		"-inbox -- id:x",
		"+a%zz -- id:x",
		"+a%2 -- id:x",
		"+a -- thread:x",
		"+a -- hash:abc",
		"+a--b id:x",
		`+a -- id:"open`,
		"+a -- id:",
	} {
		if _, err := ix.Restore(t.Context(), strings.NewReader(line+"\n")); err == nil {
			t.Errorf("Restore(%q) accepted", line)
		}
	}
}

func TestQuoteID(t *testing.T) {
	for _, id := range []string{"plain@x", `with space@x`, `q"uote@x`, "(paren)@x"} {
		got, err := unquoteID(quoteID(id))
		if err != nil || got != id {
			t.Errorf("round trip %q = %q, %v", id, got, err)
		}
	}
}

func FuzzRestoreLine(f *testing.F) {
	f.Add("+inbox +a%20b -- id:\"x y\"")
	f.Add("-- hash:" + hash(1))
	f.Fuzz(func(t *testing.T, line string) {
		tags, _, key, err := parseDumpLine(line)
		if err != nil {
			return
		}
		for _, tag := range tags {
			if ValidateTag(tag) != nil {
				t.Fatalf("invalid tag %q accepted", tag)
			}
			if got, err := decodeTag(encodeTag(tag)); err != nil || got != tag {
				t.Fatalf("tag round trip %q -> %q", tag, got)
			}
		}
		if key.field == "id" {
			if got, err := unquoteID(quoteID(key.value)); err != nil || got != key.value {
				t.Fatalf("id round trip %q -> %q (%v)", key.value, got, err)
			}
		}
	})
}
