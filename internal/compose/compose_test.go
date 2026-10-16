// SPDX-License-Identifier: AGPL-3.0-or-later

package compose

import (
	"slices"
	"strings"
	"testing"
)

func TestReplyRecipients(t *testing.T) {
	me := Me{Addresses: []string{"me@example.org", "me@mydomain.org"}}
	o := Original{From: "Alice <alice@example.org>", To: []string{"me@example.org", "carol@example.org"},
		Cc: []string{"Me Too <ME@mydomain.org>", "dave@example.org"}, Subject: "Re: RE: Lunch", MessageID: "m1@x",
		References: []string{"r0@x"}, Body: "Pizza?\n", Date: "Wed, 14 Oct 2026 09:00"}
	r := Reply(o, me, false)
	if !slices.Equal(r.To, []string{"Alice <alice@example.org>"}) || len(r.Cc) != 0 || r.Subject != "Re: Lunch" {
		t.Errorf("reply = %+v", r)
	}
	if r.InReplyTo != "m1@x" || !slices.Equal(r.References, []string{"r0@x", "m1@x"}) {
		t.Errorf("threading = %q %v", r.InReplyTo, r.References)
	}
	all := Reply(o, me, true)
	if !slices.Equal(all.To, []string{"Alice <alice@example.org>"}) ||
		!slices.Equal(all.Cc, []string{"carol@example.org", "dave@example.org"}) {
		t.Errorf("reply all = to %v cc %v (own addresses excluded)", all.To, all.Cc)
	}
	// Mail to an identity is answered from it; mail to an alias seen only in To is not.
	if f := Reply(Original{From: "a@b.c", To: []string{"me@mydomain.org"}}, me, false).From; f != "me@mydomain.org" {
		t.Errorf("from = %q, want the identity", f)
	}
	if f := Reply(Original{From: "a@b.c", To: []string{"shop@alias.addy.io"}}, me, false).From; f != "me@example.org" {
		t.Errorf("from = %q, want the account address (never an alias seen in To)", f)
	}
}

func TestReplyToAndAliasServices(t *testing.T) {
	me := Me{Addresses: []string{"me@example.org"}}
	o := Original{From: "Shop <noreply@shop.example>", ReplyTo: []string{"shop_at_example_x7@reply.addy.io"},
		To: []string{"myalias@addy.io"}, Subject: "Order", MessageID: "o@x"}
	r := Reply(o, me, false)
	if !slices.Equal(r.To, []string{"shop_at_example_x7@reply.addy.io"}) || r.From != "me@example.org" {
		t.Errorf("reply = to %v from %q", r.To, r.From)
	}
}

func TestQuoteSignatureForward(t *testing.T) {
	me := Me{Addresses: []string{"me@example.org"}, Signature: "me"}
	o := Original{From: "Alice <alice@example.org>", Date: "Wed, 14 Oct 2026 09:00", Subject: "Fwd: Plan",
		Body: "line one\n> older\n\n", MessageID: "m@x"}
	r := Reply(o, me, false)
	want := "\n\nOn Wed, 14 Oct 2026 09:00, Alice wrote:\n> line one\n>> older\n\n-- \nme\n"
	if r.Body != want {
		t.Errorf("reply body = %q, want %q", r.Body, want)
	}
	f := Forward(o, me)
	if f.Subject != "Fwd: Plan" || len(f.To) != 0 || !strings.Contains(f.Body,
		"---------- Forwarded message ----------\nFrom: Alice <alice@example.org>\nDate: Wed, 14 Oct 2026 09:00\nSubject: Fwd: Plan\n\nline one\n") {
		t.Errorf("forward = %+v", f)
	}
	if f.InReplyTo != "" {
		t.Error("a forward is not a reply")
	}
	n := New(Me{Addresses: []string{"me@example.org"}, Name: "Me", Signature: "me"}, []string{"a@example.org"})
	if n.From != "Me <me@example.org>" || !slices.Equal(n.To, []string{"a@example.org"}) || n.Body != "\n\n-- \nme\n" {
		t.Errorf("new = %+v", n)
	}
}

func TestSubjects(t *testing.T) {
	for in, want := range map[string]string{"Hello": "Re: Hello", "Re: Hello": "Re: Hello", "RE: re: Aw: Hi": "Re: Hi",
		"": "Re: ", "Fwd: x": "Re: Fwd: x"} {
		if got := replySubject(in); got != want {
			t.Errorf("replySubject(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"x": "Fwd: x", "Fwd: x": "Fwd: x", "FW: x": "Fwd: x"} {
		if got := forwardSubject(in); got != want {
			t.Errorf("forwardSubject(%q) = %q, want %q", in, got, want)
		}
	}
}

// Review: a display name with a comma stays one recipient through the
// editor file, and a name cannot smuggle in another address.
func TestReplyQuotesDisplayNames(t *testing.T) {
	me := Me{Addresses: []string{"me@example.org"}}
	tests := []struct {
		o    Original
		want string
	}{
		{Original{From: "Doe, John <j@x.org>", To: []string{"me@example.org"}}, `"Doe, John" <j@x.org>`},
		{Original{From: "a@x.org", ReplyTo: []string{"boss@corp.com, x <attacker@evil.com>"}}, `"boss@corp.com, x" <attacker@evil.com>`},
		{Original{From: `"Quote \"Q\" Smith" <q@x.org>`}, `"Quote \"Q\" Smith" <q@x.org>`},
	}
	for _, tt := range tests {
		d := Reply(tt.o, me, true)
		back, err := Parse(Render(d), d)
		if err != nil || !slices.Equal(back.To, []string{tt.want}) || len(back.Cc) != 0 {
			t.Errorf("reply to %q: To %q, Cc %q (%v), want %q", tt.o.From, back.To, back.Cc, err, tt.want)
		}
	}
	// Your own address is recognised even behind such a name.
	d := Reply(Original{From: "a@x.org", To: []string{"Me, Myself <me@example.org>", "b@x.org"}}, me, true)
	if !slices.Equal(d.Cc, []string{"b@x.org"}) {
		t.Errorf("cc = %q", d.Cc)
	}
}

// A note to yourself (from you, to you) is answered to you; mail you sent
// to others still goes to them.
func TestReplyToANoteToSelf(t *testing.T) {
	me := Me{Addresses: []string{"me@example.org", "me@mydomain.org"}}
	tests := []struct {
		o    Original
		want []string
	}{
		{Original{From: "Me <me@example.org>", To: []string{"me@example.org"}}, []string{"Me <me@example.org>"}},
		{Original{From: "me@example.org", To: []string{"me@mydomain.org"}}, []string{"me@example.org"}},
		{Original{From: "me@example.org", To: []string{"alice@example.org"}}, []string{"alice@example.org"}},
	}
	for _, tt := range tests {
		if d := Reply(tt.o, me, false); !slices.Equal(d.To, tt.want) {
			t.Errorf("reply to %+v: To %q, want %q", tt.o, d.To, tt.want)
		}
	}
}
