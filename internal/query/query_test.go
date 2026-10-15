// SPDX-License-Identifier: AGPL-3.0-or-later

package query

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want Query
	}{
		{"", Query{}},
		{"   ", Query{}},
		{"hello", Query{[][]Term{{{Field: Text, Value: "hello"}}}}},
		{"tag:todo from:bob", Query{[][]Term{{{Field: Tag, Value: "todo"}}, {{Field: From, Value: "bob"}}}}},
		{"tag:a OR tag:b from:x", Query{[][]Term{
			{{Field: Tag, Value: "a"}, {Field: Tag, Value: "b"}},
			{{Field: From, Value: "x"}},
		}}},
		{"-is:Unread", Query{[][]Term{{{Field: Is, Value: "unread", Not: true}}}}},
		{`subject:"quarterly report"`, Query{[][]Term{{{Field: Subject, Value: "quarterly report"}}}}},
		{`"tag:not a field"`, Query{[][]Term{{{Field: Text, Value: "tag:not a field"}}}}},
		{`-"re: hello"`, Query{[][]Term{{{Field: Text, Value: "re: hello", Not: true}}}}},
		{`"OR"`, Query{[][]Term{{{Field: Text, Value: "OR"}}}}},
		{`"-5 degrees"`, Query{[][]Term{{{Field: Text, Value: "-5 degrees"}}}}},
		{`-"-5 degrees"`, Query{[][]Term{{{Field: Text, Value: "-5 degrees", Not: true}}}}},
		{"id:<abc@x>", Query{[][]Term{{{Field: ID, Value: "abc@x"}}}}},
		{"hash:ABCDEF12", Query{[][]Term{{{Field: Hash, Value: "abcdef12"}}}}},
		{`"say \"hi\""`, Query{[][]Term{{{Field: Text, Value: `say "hi"`}}}}},
		{"id:abc:def@x", Query{[][]Term{{{Field: ID, Value: "abc:def@x"}}}}},
		{"FROM:bob", Query{[][]Term{{{Field: From, Value: "bob"}}}}},
		{":odd", Query{[][]Term{{{Field: Text, Value: ":odd"}}}}},
		{"-", Query{[][]Term{{{Field: Text, Value: "-"}}}}},
		{"date:2026-01-01..2026-03-31", Query{[][]Term{{{Field: Date, Value: "2026-01-01..2026-03-31",
			Since: day("2026-01-01"), Before: day("2026-04-01")}}}}},
		{"date:2026-05-02", Query{[][]Term{{{Field: Date, Value: "2026-05-02",
			Since: day("2026-05-02"), Before: day("2026-05-03")}}}}},
		{"date:2026-05-02..", Query{[][]Term{{{Field: Date, Value: "2026-05-02..", Since: day("2026-05-02")}}}}},
		{"date:..2026-05-02", Query{[][]Term{{{Field: Date, Value: "..2026-05-02", Before: day("2026-05-03")}}}}},
	}
	for _, tt := range tests {
		got, err := Parse(tt.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Parse(%q) =\n%+v\nwant\n%+v", tt.in, got, tt.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]string{
		"OR a":                        "OR must be",
		"a OR":                        "OR must be",
		"a OR OR b":                   "OR must be",
		"bogus:x":                     "unknown field",
		"tag:":                        "needs a value",
		"is:sleepy":                   "expected one of",
		"date:yesterday":              "YYYY-MM-DD",
		"date:2026-01-01..soon":       "YYYY-MM-DD",
		"date:..":                     "at least one end",
		"date:2026-03-01..2026-01-01": "ends before",
		`"open`:                       "unterminated",
		`""`:                          "empty term",
		strings.Repeat("a", MaxLen+1): "too long",
		"\xff":                        "UTF-8",
	}
	for in, want := range tests {
		_, err := Parse(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) error = %v, want it to contain %q", in, err, want)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, in := range []string{
		`tag:a OR tag:b -from:"Bob Smith" "re: hi" -"-dash" "OR" date:2026-01-01..`,
		`subject:"back\\slash" id:x@y is:inbox`,
		`"-literal" -"-negated literal" -word`,
	} {
		q, err := Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Parse(q.String())
		if err != nil {
			t.Fatalf("reparse %q: %v", q.String(), err)
		}
		if !reflect.DeepEqual(q, again) {
			t.Errorf("round trip of %q via %q changed:\n%+v\n%+v", in, q.String(), q, again)
		}
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"tag:a OR b", `"x y" -is:unread`, "date:2026-01-01..", `a\"b`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q, err := Parse(s)
		if err != nil {
			return
		}
		again, err := Parse(q.String())
		if err != nil {
			t.Fatalf("Parse(%q) ok but reparse of %q failed: %v", s, q.String(), err)
		}
		if !reflect.DeepEqual(q, again) {
			t.Fatalf("round trip of %q via %q changed", s, q.String())
		}
	})
}
