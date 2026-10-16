// SPDX-License-Identifier: AGPL-3.0-or-later

// Package compose builds outgoing mail: replies, forwards and new
// messages as an editor file, and the checks shown before sending. It is
// pure: no I/O, no clock; callers pass in what it needs.
package compose

import (
	"net/mail"
	"regexp"
	"slices"
	"strings"
)

// Original is the message being answered or forwarded.
type Original struct {
	From            string   // "Name <addr>"
	Date            string   // preformatted by the caller, e.g. "Wed, 14 Oct 2026 09:00"
	ReplyTo, To, Cc []string // "Name <addr>" entries
	Subject         string
	MessageID       string   // bare, without <>
	References      []string // bare
	Body            string   // plain text (HTML already converted by the caller)
}

// Me is the sending account.
type Me struct {
	Addresses []string // own addresses, lowercased; [0] is the default sender
	Name      string   // display name for From, may be empty
	Signature string
}

// Draft is a message being written.
type Draft struct {
	From       string   `json:"from"`
	To         []string `json:"to"`
	Cc         []string `json:"cc,omitempty"`
	Bcc        []string `json:"bcc,omitempty"`
	Subject    string   `json:"subject"`
	Attach     []string `json:"attach,omitempty"` // file paths as written (~ not expanded)
	Body       string   `json:"body"`
	InReplyTo  string   `json:"in_reply_to,omitempty"` // hidden from the editor file
	References []string `json:"references,omitempty"`  // hidden from the editor file
}

const forwardMarker = "---------- Forwarded message ----------"

// New starts a message to the given recipients.
func New(me Me, to []string) Draft {
	return Draft{From: me.from(me.Addresses[0]), To: to, Body: "\n" + signature(me)}
}

// Reply answers o: to Reply-To, else From; with all, also everyone in To
// and Cc except the user's own addresses. It is sent from the address the
// mail was sent to if that is one of the user's identities, else from the
// default address: never from an alias that only appears in To. Alias
// services (addy.io, SimpleLogin) put a reverse alias in Reply-To or From
// and rewrite the reply into the alias themselves.
func Reply(o Original, me Me, all bool) Draft {
	to := withoutOwn(o.ReplyTo, me)
	if len(to) == 0 {
		to = withoutOwn([]string{o.From}, me)
	}
	if len(to) == 0 { // a reply to your own message goes to its recipients
		to = withoutOwn(o.To, me)
	}
	if len(to) == 0 && o.From != "" { // a note to yourself: back to you
		to = []string{o.From}
	}
	d := Draft{From: sender(o, me), To: recipients(dedupe(to, nil)), Subject: replySubject(o.Subject),
		InReplyTo: o.MessageID, References: appendRef(o.References, o.MessageID)}
	if all {
		d.Cc = recipients(dedupe(withoutOwn(slices.Concat(o.To, o.Cc), me), d.To))
	}
	d.Body = "\n\n" + attribution(o) + quote(o.Body) + signature(me)
	return d
}

// Forward passes o on inline: its main headers and its text. The caller
// attaches the original's attachments again when the message is built.
func Forward(o Original, me Me) Draft {
	var b strings.Builder
	b.WriteString("\n\n" + forwardMarker + "\nFrom: " + o.From + "\nDate: " + o.Date + "\nSubject: " + o.Subject + "\n")
	if len(o.To) > 0 {
		b.WriteString("To: " + strings.Join(o.To, ", ") + "\n")
	}
	b.WriteString("\n" + o.Body)
	if !strings.HasSuffix(o.Body, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(signature(me))
	return Draft{From: sender(o, me), Subject: forwardSubject(o.Subject), Body: b.String()}
}

var (
	replyPrefix = regexp.MustCompile(`(?i)^\s*((re|aw|sv|antw)\s*:\s*)+`)
	fwdPrefix   = regexp.MustCompile(`(?i)^\s*((fwd?|wg|tr)\s*:\s*)+`)
)

func replySubject(s string) string   { return "Re: " + replyPrefix.ReplaceAllString(s, "") }
func forwardSubject(s string) string { return "Fwd: " + fwdPrefix.ReplaceAllString(s, "") }

func (me Me) from(addr string) string { return format(me.Name, addr) }

// format writes an address for a header line, quoting the name when it
// needs it: a comma in "Doe, John" must not split the list.
func format(name, addr string) string {
	switch {
	case name == "":
		return addr
	case plainName.MatchString(name):
		return name + " <" + addr + ">"
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `" <` + addr + ">"
}

// split reads "Name <addr>". Names from the index are not quoted, so a
// name with a comma or an @ is read up to the last angle bracket.
func split(s string) (name, addr string) {
	if a, err := mail.ParseAddress(s); err == nil {
		return a.Name, a.Address
	}
	i, j := strings.LastIndex(s, "<"), strings.LastIndex(s, ">")
	if i < 0 || j < i {
		return "", ""
	}
	return strings.Trim(strings.TrimSpace(s[:i]), `"`), strings.TrimSpace(s[i+1 : j])
}

// recipients formats entries for a header line; one that cannot be read
// is kept as it is, for the checks to report.
func recipients(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if name, addr := split(s); addr != "" {
			s = format(name, addr)
		}
		out = append(out, s)
	}
	return out
}

// plainName is a display name that needs no quoting.
var plainName = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+/=?^_{|}~ -]+$`)

// sender is the identity the original was sent to, if any, else the
// default address.
func sender(o Original, me Me) string {
	for _, s := range slices.Concat(o.To, o.Cc) {
		if a := address(s); a != "" && slices.Contains(me.Addresses[1:], a) {
			return me.from(a)
		}
	}
	return me.from(me.Addresses[0])
}

// address is the lowercased address of an entry, or "" if it does not
// parse.
func address(s string) string {
	_, addr := split(s)
	return strings.ToLower(addr)
}

func withoutOwn(list []string, me Me) []string {
	return slices.DeleteFunc(slices.Clone(list), func(s string) bool { return slices.Contains(me.Addresses, address(s)) })
}

// dedupe drops repeated addresses, and those already in also.
func dedupe(list, also []string) []string {
	seen := map[string]bool{}
	for _, s := range also {
		seen[key(s)] = true
	}
	var out []string
	for _, s := range list {
		if k := key(s); !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

func key(s string) string {
	if a := address(s); a != "" {
		return a
	}
	return strings.ToLower(strings.TrimSpace(s))
}

func appendRef(refs []string, id string) []string {
	out := slices.Clone(refs)
	if id != "" && (len(out) == 0 || out[len(out)-1] != id) {
		out = append(out, id)
	}
	return out
}

func attribution(o Original) string {
	name := o.From
	if a, err := mail.ParseAddress(o.From); err == nil {
		name = a.Name
		if name == "" {
			name = a.Address
		}
	}
	if o.Date == "" {
		return name + " wrote:\n"
	}
	return "On " + o.Date + ", " + name + " wrote:\n"
}

// quote prefixes each line with "> " (">" for lines already quoted).
func quote(body string) string {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return ""
	}
	var b strings.Builder
	for line := range strings.SplitSeq(body, "\n") {
		switch {
		case strings.HasPrefix(line, ">"):
			b.WriteString(">" + line + "\n")
		case line == "":
			b.WriteString(">\n")
		default:
			b.WriteString("> " + line + "\n")
		}
	}
	return b.String()
}

// Signed is body with the signature, as new messages get it.
func Signed(body string, me Me) string {
	return strings.TrimRight(body, "\n") + "\n" + signature(me)
}

func signature(me Me) string {
	if me.Signature == "" {
		return ""
	}
	return "\n-- \n" + me.Signature + "\n"
}
