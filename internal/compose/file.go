// SPDX-License-Identifier: AGPL-3.0-or-later

package compose

import (
	"errors"
	"strings"
)

const fileHint = "# Lines starting with # are removed. Save and quit to review; quit without saving to cancel."

// Render is the editor file: the visible headers, a hint, a blank line and
// the body.
func Render(d Draft) string {
	var b strings.Builder
	b.WriteString("From: " + d.From + "\nTo: " + strings.Join(d.To, ", ") + "\nCc: " + strings.Join(d.Cc, ", ") +
		"\nBcc: " + strings.Join(d.Bcc, ", ") + "\nSubject: " + d.Subject + "\nAttach: " + strings.Join(d.Attach, ", ") +
		"\n" + fileHint + "\n\n" + d.Body)
	return b.String()
}

// Parse reads an edited file. Headers run to the first blank line; lines
// starting with # and unknown headers are dropped. The hidden fields
// (threading) are kept from base. CRLF line endings (Windows editors) are
// read as LF.
func Parse(text string, base Draft) (Draft, error) {
	head, body, _ := strings.Cut(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n")
	if strings.ContainsRune(head, '\r') {
		return Draft{}, errors.New("the header lines contain a carriage return")
	}
	d := Draft{InReplyTo: base.InReplyTo, References: base.References, Body: body}
	for line := range strings.SplitSeq(head, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "from":
			d.From = value
		case "to":
			d.To = append(d.To, SplitAddresses(value)...)
		case "cc":
			d.Cc = append(d.Cc, SplitAddresses(value)...)
		case "bcc":
			d.Bcc = append(d.Bcc, SplitAddresses(value)...)
		case "subject":
			d.Subject = value
		case "attach":
			for p := range strings.SplitSeq(value, ",") {
				if p = strings.TrimSpace(p); p != "" {
					d.Attach = append(d.Attach, p)
				}
			}
		}
	}
	return d, nil
}

// SplitAddresses splits a comma-separated list, keeping commas inside
// quoted display names and angle brackets.
func SplitAddresses(s string) []string {
	var out []string
	var cur strings.Builder
	quoted, angle := false, false
	flush := func() {
		if v := strings.TrimSpace(cur.String()); v != "" {
			out = append(out, v)
		}
		cur.Reset()
	}
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
		case r == '<' && !quoted:
			angle = true
		case r == '>' && !quoted:
			angle = false
		case r == ',' && !quoted && !angle:
			flush()
			continue
		}
		cur.WriteRune(r)
	}
	flush()
	return out
}
