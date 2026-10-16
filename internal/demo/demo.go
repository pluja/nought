// SPDX-License-Identifier: AGPL-3.0-or-later

// Package demo is a made-up mailbox, Sam Rivera's, for the README's
// screenshots and demo. Everyone in it is invented and every domain is a
// reserved example domain: it is never anybody's real mail.
package demo

import (
	"regexp"
	"strings"
	"time"
)

// Message is one demo message.
type Message struct {
	Folder  string // the IMAP folder it is on
	Subject string
	Date    time.Time
	Read    bool
	Raw     string // CRLF line endings
}

// Owner is the mailbox owner's address.
const Owner = "Sam Rivera <sam@example.org>"

// Mailbox returns the demo mail, dated relative to now.
func Mailbox(now time.Time) []Message {
	out := make([]Message, 0, len(mail))
	for _, m := range mail {
		date := now.Add(-m.age)
		out = append(out, Message{Folder: m.folder, Subject: m.subject, Date: date, Read: m.read, Raw: m.raw(date)})
	}
	return out
}

type message struct {
	folder, from, subject, body string
	age                         time.Duration
	read, html                  bool
	attach                      string // a PDF attachment's file name
}

var notWord = regexp.MustCompile(`\W+`)

func (m message) raw(date time.Time) string {
	id := strings.ToLower(notWord.ReplaceAllString(m.subject, "")) + "@demo.example"
	head := "From: " + m.from + "\r\nTo: " + Owner + "\r\nMessage-ID: <" + id + ">\r\nDate: " +
		date.Format(time.RFC1123Z) + "\r\nSubject: " + m.subject + "\r\nMIME-Version: 1.0\r\n"
	body := strings.ReplaceAll(m.body, "\n", "\r\n")
	switch {
	case m.html:
		return head + "Content-Type: text/html; charset=utf-8\r\n\r\n" + body
	case m.attach != "":
		return head + "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
			body + "\r\n--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=" + m.attach +
			"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n--b--\r\n"
	}
	return head + "Content-Type: text/plain; charset=utf-8\r\n\r\n" + body
}

const hour = time.Hour

var mail = []message{
	{folder: "INBOX", from: "Alice Martin <alice@example.org>", subject: "Re: Weekend in Lisbon", age: 2 * hour, body: `That sounds perfect. I'll book the tram tour for Saturday morning,
and dinner at the place with the view on Friday.

Alice

On Tue, 13 Oct 2026 18:40, Sam Rivera wrote:
> Great! The hotel is in Alfama, five minutes from the viewpoint.
> Shall we do the tram tour?
>
> On Tue, 13 Oct 2026 09:15, Alice Martin wrote:
> > Flights are booked: we land on Friday at 10:05.
> > Can you find a place to stay?
`},
	{folder: "INBOX", from: "Bob Chen <bob@example.net>", subject: "Photos from Saturday", age: 5 * hour,
		body: "Here they are! The one at the lighthouse is my favourite.\n\nBob\n"},
	{folder: "INBOX", from: "The Weekly Byte <news@weeklybyte.example>", subject: "Issue #42: the return of the terminal", age: 20 * hour, html: true, body: `<html><body>
<h1>The return of the terminal</h1>
<p>Hello reader,</p>
<p>This week: why <b>keyboard-first</b> tools are everywhere again, and three you should try.</p>
<ul><li><a href="https://weeklybyte.example/a/1?utm_source=newsletter&amp;utm_medium=email">Plain text is a feature</a></li>
<li><a href="https://weeklybyte.example/a/2?utm_source=newsletter">Inbox Zero, revisited</a></li>
<li><a href="https://weeklybyte.example/a/3?utm_source=newsletter">Your files, your formats</a></li></ul>
<img src="https://tracker.weeklybyte.example/open.gif" width="1" height="1">
<p>Read the full issue on <a href="https://weeklybyte.example/42">our website</a>.</p>
<p style="font-size:11px;color:#999">You get this because you subscribed. <a href="https://weeklybyte.example/unsubscribe">Unsubscribe</a>.</p>
</body></html>`},
	{folder: "INBOX", from: "Hosting Co <billing@hostingco.example>", subject: "Invoice 2026-1015 (paid)", age: 26 * hour, read: true,
		attach: "invoice-2026-1015.pdf", body: "Thank you for your payment of 12.00 EUR.\n\nYour invoice is attached.\n"},
	{folder: "INBOX", from: "City Library <noreply@library.example>", subject: "Your books are due on Friday", age: 2 * 24 * hour, read: true,
		body: "Two books are due on Friday. Renew them online.\n"},
	{folder: "INBOX", from: "Mia Okafor <mia@example.com>", subject: "Lunch next week?", age: 3 * 24 * hour, read: true,
		body: "Tuesday or Thursday work for me. The new ramen place?\n\nMia\n"},
	{folder: "INBOX", from: "Acme Accounts <no-reply@accounts.example>", subject: "Your sign-in code", age: 30 * time.Minute, read: true,
		body: "Your code is 481 516. It expires in 10 minutes.\n"},
	{folder: "Sent", from: Owner, subject: "Re: Weekend in Lisbon (hotel)", age: 42 * hour, read: true,
		body: "Great! The hotel is in Alfama, five minutes from the viewpoint.\n"},
	{folder: "Archive", from: "Leo Park <leo@example.net>", subject: "Board game night", age: 9 * 24 * hour, read: true,
		body: "Bring snacks!\n"},
}
