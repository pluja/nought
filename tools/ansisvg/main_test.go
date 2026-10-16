// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	runs, width := parse("a\x1b[1;34mb│\x1b[0m \x1b[2;48;5;8m★\x1b[39;49m.")
	want := []run{
		{col: 0, width: 1, text: "a"},
		{col: 1, width: 2, text: "b│", st: style{bold: true, fg: palette[4]}},
		{col: 3, width: 1, text: " "},
		{col: 4, width: 1, text: "★", st: style{faint: true, bg: palette[8]}},
		{col: 5, width: 1, text: ".", st: style{faint: true}},
	}
	if width != 6 || len(runs) != len(want) {
		t.Fatalf("width %d, runs %+v", width, runs)
	}
	for i := range want {
		if runs[i] != want[i] {
			t.Errorf("run %d = %+v, want %+v", i, runs[i], want[i])
		}
	}
}

func TestDrawEscapesText(t *testing.T) {
	svg := draw("<b>&\x1b[38;2;1;2;3mx", "nou")
	if !strings.Contains(svg, "&lt;b&gt;&amp;") || !strings.Contains(svg, `fill="#010203"`) {
		t.Errorf("svg:\n%s", svg)
	}
}
