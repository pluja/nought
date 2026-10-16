// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"cmp"
	"context"
	"fmt"
	"net/mail"
	"slices"
	"strings"
)

// AddressCount is a correspondent and how much they matter.
type AddressCount struct {
	Address string `json:"address"` // lowercased
	Display string `json:"display"` // "Name <addr>" as last seen, or the address
	Count   int    `json:"count"`
}

// addressScan bounds the messages read for correspondents (newest first).
const addressScan = 20000

// Addresses lists an account's correspondents, most used first: people you
// wrote to (the To and Cc of sent mail) count double, senders of mail you
// received count once.
func (ix *Index) Addresses(ctx context.Context, account string, limit int) ([]AddressCount, error) {
	rows, err := ix.db.QueryContext(ctx, `
		SELECT m.from_addr, m.to_addrs,
		       EXISTS (SELECT 1 FROM tags t WHERE t.message_id = m.id AND t.tag = 'folder:sent')
		FROM messages m JOIN accounts a ON a.id = m.account_id
		WHERE a.name = ? ORDER BY m.date DESC LIMIT ?`, account, addressScan)
	if err != nil {
		return nil, fmt.Errorf("addresses: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byAddr := map[string]*AddressCount{}
	count := func(entry string, weight int) {
		a, err := mail.ParseAddress(entry)
		if err != nil {
			return
		}
		key := strings.ToLower(a.Address)
		c, ok := byAddr[key]
		if !ok {
			c = &AddressCount{Address: key, Display: entry} // rows are newest first: keep the first form seen
			byAddr[key] = c
		}
		c.Count += weight
	}
	for rows.Next() {
		var from, to string
		var sent bool
		if err := rows.Scan(&from, &to, &sent); err != nil {
			return nil, err
		}
		if !sent {
			count(from, 1)
			continue
		}
		for entry := range strings.SplitSeq(to, "\n") {
			count(entry, 2)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]AddressCount, 0, len(byAddr))
	for _, c := range byAddr {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(x, y AddressCount) int {
		return cmp.Or(cmp.Compare(y.Count, x.Count), cmp.Compare(x.Address, y.Address))
	})
	return out[:min(len(out), limit)], nil
}
