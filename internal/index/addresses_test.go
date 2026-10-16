// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAddresses(t *testing.T) {
	ix, _ := openIndex(t)
	ctx := t.Context()
	now := time.Now()
	add := func(hash, from string, to []string, tags []string) {
		t.Helper()
		if _, _, err := ix.AddMessage(ctx, Message{Account: "a", Hash: hash, From: from, To: to, Tags: tags}, "", now); err != nil {
			t.Fatal(err)
		}
	}
	add(strings.Repeat("1", 64), "Alice <alice@example.org>", []string{"me@example.org"}, []string{"inbox"})
	add(strings.Repeat("2", 64), "me@example.org", []string{"Bob <BOB@example.org>", "not an address"}, []string{"folder:sent"})
	add(strings.Repeat("3", 64), "Alice <alice@example.org>", []string{"me@example.org"}, nil)
	got, err := ix.Addresses(ctx, "a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 2 || got[0].Address != "alice@example.org" || got[0].Display != "Alice <alice@example.org>" {
		t.Errorf("addresses = %+v", got)
	}
	if !slices.ContainsFunc(got, func(a AddressCount) bool { return a.Address == "bob@example.org" && a.Count == 2 }) {
		t.Errorf("a sent-to address is not counted double: %+v", got)
	}
	if other, _ := ix.Addresses(ctx, "b", 10); len(other) != 0 {
		t.Errorf("another account's addresses leaked: %+v", other)
	}
}
