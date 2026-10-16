// SPDX-License-Identifier: AGPL-3.0-or-later

package compose

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestChecks(t *testing.T) {
	me := Me{Addresses: []string{"me@example.org"}}
	size := func(p string) (int64, error) {
		switch p {
		case "/ok.pdf":
			return 1000, nil
		case "/dir":
			return 0, errors.New("is a directory")
		}
		return 0, os.ErrNotExist
	}
	in := func(d Draft) CheckInput {
		return CheckInput{Draft: d, Me: me, Known: []string{"alice@example.org"}, Attach: size, SizeLimit: 25 << 20}
	}
	base := Draft{From: "me@example.org", To: []string{"alice@example.org"}, Subject: "s", Body: "hi"}
	cases := []struct {
		name     string
		edit     func(*Draft)
		blocking bool
		text     string
	}{
		{"no recipients", func(d *Draft) { d.To = nil }, true, "no recipients"},
		{"unknown from", func(d *Draft) { d.From = "x@evil.example" }, true, "not one of your addresses"},
		{"missing attachment", func(d *Draft) { d.Attach = []string{"/nope"} }, true, "/nope"},
		{"directory attachment", func(d *Draft) { d.Attach = []string{"/dir"} }, true, "/dir"},
		{"attachment under the limit", func(d *Draft) { d.Attach = []string{"/ok.pdf"} }, false, ""},
		{"empty subject", func(d *Draft) { d.Subject = "" }, false, "empty subject"},
		{"forgot attachment", func(d *Draft) { d.Body = "please find the file attached" }, false, "nothing is attached"},
		{"forgot attachment es", func(d *Draft) { d.Body = "te envío el documento adjunto" }, false, "nothing is attached"},
		{"attachment word only in a quote", func(d *Draft) { d.Body = "ok\n> see attached\n" }, false, ""},
		{"lookalike", func(d *Draft) { d.To = []string{"alice@exarnple.org"} }, false, "looks like alice@example.org"},
	}
	for _, c := range cases {
		d := base
		c.edit(&d)
		ps := Check(in(d))
		if c.text == "" {
			if len(ps) != 0 {
				t.Errorf("%s: unexpected %+v", c.name, ps)
			}
			continue
		}
		if !slices.ContainsFunc(ps, func(p Problem) bool { return p.Blocking == c.blocking && strings.Contains(p.Text, c.text) }) {
			t.Errorf("%s: got %+v", c.name, ps)
		}
	}
	big := in(base)
	big.Draft.Attach, big.SizeLimit = []string{"/ok.pdf"}, 500
	if ps := Check(big); !slices.ContainsFunc(ps, func(p Problem) bool { return p.Blocking && strings.Contains(p.Text, "too large") }) {
		t.Errorf("size limit: %+v", ps)
	}
	many := in(base)
	many.ReplyAll = true
	for i := range 11 {
		many.Draft.Cc = append(many.Draft.Cc, fmt.Sprintf("p%d@example.org", i))
	}
	if ps := Check(many); !slices.ContainsFunc(ps, func(p Problem) bool { return strings.Contains(p.Text, "12 people") }) {
		t.Errorf("reply-all size: %+v", ps)
	}
}

// Deferred (f): a reply that goes to a Reply-To at another domain than
// the sender's says so: a mailing list, or a trick.
func TestReplyToElsewhereWarns(t *testing.T) {
	me := Me{Addresses: []string{"me@example.org"}}
	o := Original{From: "Bank <alerts@bank.example>", ReplyTo: []string{"Bank <help@evil.example>"}}
	warned := func(to []string, o Original) bool {
		d := Draft{From: "me@example.org", To: to, Subject: "s", Body: "b"}
		in := CheckInput{Draft: d, Me: me, Original: &o, Attach: func(string) (int64, error) { return 0, nil }}
		return slices.ContainsFunc(Check(in), func(p Problem) bool { return !p.Blocking && strings.Contains(p.Text, "Reply-To") })
	}
	if !warned([]string{"Bank <help@evil.example>"}, o) {
		t.Error("no warning for a Reply-To at another domain")
	}
	if warned([]string{"alerts@bank.example"}, o) {
		t.Error("warned though the reply goes to the sender")
	}
	same := Original{From: "a@bank.example", ReplyTo: []string{"help@bank.example"}}
	if warned([]string{"help@bank.example"}, same) {
		t.Error("warned for a Reply-To at the sender's domain")
	}
}
