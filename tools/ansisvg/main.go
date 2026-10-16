// SPDX-License-Identifier: AGPL-3.0-or-later

// Command ansisvg draws a terminal screen (text with ANSI colors, on stdin)
// as an SVG terminal window (on stdout), for the README screenshots. It
// understands the SGR codes the TUI emits: 16, 256 and true colors, bold,
// faint, italic, underline and reverse.
//
//	go run ./tools/ansisvg -title "nou" < inbox.ansi > inbox.svg
package main

import (
	"bufio"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

// Tokyo Night: the 16 palette colors, then the default background and text.
var palette = [16]string{
	"#15161e", "#f7768e", "#9ece6a", "#e0af68", "#7aa2f7", "#bb9af7", "#7dcfff", "#a9b1d6",
	"#414868", "#ff899d", "#9fe044", "#faba4a", "#8db0ff", "#c7a9ff", "#a4daff", "#c0caf5",
}

const (
	background = "#1a1b26"
	foreground = "#c0caf5"
	cellW      = 8.43 // a 14px monospace cell
	lineH      = 19.0
	pad        = 18.0
	titleH     = 34.0
)

type style struct {
	fg, bg                                   string
	bold, faint, italic, underline, reversed bool
}

type run struct {
	col, width int
	text       string
	st         style
}

func main() {
	title := flag.String("title", "nou", "window title")
	flag.Parse()
	in, err := io.ReadAll(bufio.NewReader(os.Stdin))
	if err != nil {
		fmt.Fprintln(os.Stderr, "ansisvg:", err)
		os.Exit(1)
	}
	if _, err := os.Stdout.WriteString(draw(string(in), *title)); err != nil {
		fmt.Fprintln(os.Stderr, "ansisvg:", err)
		os.Exit(1)
	}
}

func draw(screen, title string) string {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	var rows [][]run
	cols := 0
	for _, line := range lines {
		r, w := parse(line)
		rows = append(rows, r)
		cols = max(cols, w)
	}
	width := 2*pad + float64(cols)*cellW
	height := titleH + 2*pad + float64(len(rows))*lineH
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`+"\n",
		width, height, width, height)
	fmt.Fprintf(&b, `<rect width="100%%" height="100%%" rx="10" fill="%s"/>`+"\n", background)
	for i, c := range []string{"#ff5f57", "#febc2e", "#28c840"} {
		fmt.Fprintf(&b, `<circle cx="%.0f" cy="%.0f" r="6" fill="%s"/>`+"\n", pad+6+float64(i)*20, titleH/2+2, c)
	}
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" fill="#787c99" font-family="sans-serif" font-size="13" text-anchor="middle">%s</text>`+"\n",
		width/2, titleH/2+6, html.EscapeString(title))
	b.WriteString(`<g font-family="'JetBrains Mono','Fira Code','SF Mono',Menlo,Consolas,'DejaVu Sans Mono',monospace" font-size="14" xml:space="preserve">` + "\n")
	for i, row := range rows {
		top := titleH + pad + float64(i)*lineH
		for _, r := range row {
			fg, bg := colors(r.st)
			if bg != "" {
				x, w := pad+float64(r.col)*cellW, float64(r.width)*cellW
				fmt.Fprintf(&b, `<rect x="%.2f" y="%.2f" width="%.2f" height="%.2f" fill="%s"/>`+"\n", x, top, w, lineH, bg)
			}
			if strings.TrimSpace(r.text) == "" && !r.st.underline {
				continue
			}
			attrs := fmt.Sprintf(`fill="%s"`, fg)
			if r.st.bold {
				attrs += ` font-weight="bold"`
			}
			if r.st.faint {
				attrs += ` fill-opacity="0.55"`
			}
			if r.st.italic {
				attrs += ` font-style="italic"`
			}
			if r.st.underline {
				attrs += ` text-decoration="underline"`
			}
			// Browsers collapse spaces in SVG text whatever xml:space says,
			// so each word is placed at its own column, and textLength pins
			// it to its cells whatever the font.
			for _, wd := range words(r) {
				fmt.Fprintf(&b, `<text x="%.2f" y="%.2f" textLength="%.2f" lengthAdjust="spacingAndGlyphs" %s>%s</text>`+"\n",
					pad+float64(wd.col)*cellW, top+lineH-5, float64(wd.width)*cellW, attrs, html.EscapeString(wd.text))
			}
		}
	}
	b.WriteString("</g>\n</svg>\n")
	return b.String()
}

// words splits a run at its spaces, each word with its column and width.
func words(r run) []run {
	var out []run
	col, cur := r.col, run{}
	for _, c := range r.text {
		w := ansi.StringWidth(string(c))
		if c == ' ' {
			if cur.text != "" {
				out = append(out, cur)
				cur = run{}
			}
		} else {
			if cur.text == "" {
				cur.col = col
			}
			cur.text += string(c)
			cur.width += w
		}
		col += w
	}
	if cur.text != "" {
		out = append(out, cur)
	}
	return out
}

// colors resolves a style to fill colors ("" bg: none).
func colors(st style) (fg, bg string) {
	fg, bg = st.fg, st.bg
	if fg == "" {
		fg = foreground
	}
	if st.reversed {
		fg, bg = bg, fg
		if fg == "" {
			fg = background
		}
	}
	return fg, bg
}

// parse splits one line into runs of equally styled text, by cell column.
func parse(line string) ([]run, int) {
	var runs []run
	var cur run
	var st style
	col := 0
	flush := func() {
		if cur.text != "" {
			runs = append(runs, cur)
		}
		cur = run{col: col, st: st}
	}
	for i := 0; i < len(line); {
		if line[i] == 0x1b && i+1 < len(line) && line[i+1] == '[' {
			j := i + 2
			for j < len(line) && (line[j] < 0x40 || line[j] > 0x7e) {
				j++
			}
			if j < len(line) && line[j] == 'm' {
				flush()
				st = sgr(st, line[i+2:j])
				cur.st = st
			}
			i = j + 1
			continue
		}
		r, size := utf8.DecodeRuneInString(line[i:])
		w := ansi.StringWidth(string(r))
		cur.text += string(r)
		cur.width += w
		col += w
		i += size
	}
	flush()
	return runs, col
}

// sgr applies one "ESC [ ... m" sequence.
func sgr(st style, params string) style {
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		code, _ := strconv.Atoi(strings.SplitN(ps[i], ":", 2)[0])
		switch {
		case code == 0 || ps[i] == "":
			st = style{}
		case code == 1:
			st.bold = true
		case code == 2:
			st.faint = true
		case code == 3:
			st.italic = true
		case code == 4:
			st.underline = !strings.HasSuffix(ps[i], ":0")
		case code == 7:
			st.reversed = true
		case code == 22:
			st.bold, st.faint = false, false
		case code == 23:
			st.italic = false
		case code == 24:
			st.underline = false
		case code == 27:
			st.reversed = false
		case code >= 30 && code <= 37:
			st.fg = palette[code-30]
		case code >= 90 && code <= 97:
			st.fg = palette[code-90+8]
		case code >= 40 && code <= 47:
			st.bg = palette[code-40]
		case code >= 100 && code <= 107:
			st.bg = palette[code-100+8]
		case code == 39:
			st.fg = ""
		case code == 49:
			st.bg = ""
		case code == 38 || code == 48:
			c, used := extended(ps[i+1:])
			i += used
			if code == 38 {
				st.fg = c
			} else {
				st.bg = c
			}
		}
	}
	return st
}

// extended reads "5;n" or "2;r;g;b" after 38 or 48.
func extended(ps []string) (string, int) {
	if len(ps) >= 2 && ps[0] == "5" {
		n, _ := strconv.Atoi(ps[1])
		return color256(n), 2
	}
	if len(ps) >= 4 && ps[0] == "2" {
		r, _ := strconv.Atoi(ps[1])
		g, _ := strconv.Atoi(ps[2])
		b, _ := strconv.Atoi(ps[3])
		return fmt.Sprintf("#%02x%02x%02x", r, g, b), 4
	}
	return "", len(ps)
}

func color256(n int) string {
	switch {
	case n < 16:
		return palette[max(n, 0)]
	case n < 232:
		n -= 16
		level := func(v int) int {
			if v == 0 {
				return 0
			}
			return 55 + v*40
		}
		return fmt.Sprintf("#%02x%02x%02x", level(n/36), level(n/6%6), level(n%6))
	default:
		v := 8 + (n-232)*10
		return fmt.Sprintf("#%02x%02x%02x", v, v, v)
	}
}
