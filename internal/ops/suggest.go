// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"slices"
	"strings"

	"github.com/pluja/nought/internal/index"
)

// SuggestIn asks for addresses starting with a prefix.
type SuggestIn struct {
	Account string `json:"account,omitempty"`
	Prefix  string `json:"prefix"`
}

// AddressSuggest completes addresses from the archive.
var AddressSuggest = Op[SuggestIn, []index.AddressCount]{
	Name: "address.suggest", Summary: "Complete an address from your archive", Perm: Read,
	Run: func(ctx context.Context, env *Env, in SuggestIn) ([]index.AddressCount, error) {
		a, err := sendingAccount(env, in.Account)
		if err != nil {
			return nil, err
		}
		ix, err := env.Index(ctx)
		if err != nil {
			return nil, err
		}
		all, err := ix.Addresses(ctx, a.Name, 2000)
		if err != nil {
			return nil, err
		}
		prefix := strings.ToLower(strings.TrimSpace(in.Prefix))
		out := []index.AddressCount{}
		for _, c := range all {
			if strings.HasPrefix(c.Address, prefix) || wordPrefix(strings.ToLower(c.Display), prefix) {
				out = append(out, c)
				if len(out) == 10 {
					break
				}
			}
		}
		return out, nil
	},
}

func wordPrefix(s, prefix string) bool {
	return slices.ContainsFunc(strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '"' || r == '<' }),
		func(w string) bool { return strings.HasPrefix(w, prefix) })
}
