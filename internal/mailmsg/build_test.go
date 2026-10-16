// SPDX-License-Identifier: AGPL-3.0-or-later

package mailmsg

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func fixedRand() *strings.Reader { return strings.NewReader(strings.Repeat("r", 64)) }

func TestBuildIsPrivateAndParsesBack(t *testing.T) {
	now := time.Date(2026, 10, 16, 9, 30, 0, 0, time.FixedZone("CEST", 7200))
	raw, id, err := Build(Outgoing{
		From: "Me <me@example.org>", To: []string{"Ålice <alice@example.org>"}, Cc: []string{"bob@example.org"},
		Bcc: []string{"secret@example.org"}, Subject: "Réunion demain", InReplyTo: "orig@x", References: []string{"root@x", "orig@x"},
		Body: strings.Repeat("word ", 40) + "\n\n> quoted line\n",
	}, now, fixedRand())
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, bad := range []string{"User-Agent", "X-Mailer", "secret@example.org", "Bcc:"} {
		if strings.Contains(s, bad) {
			t.Errorf("sent bytes contain %q:\n%s", bad, s)
		}
	}
	for _, want := range []string{"Date: Fri, 16 Oct 2026 07:30:00 +0000", "format=flowed", "In-Reply-To: <orig@x>",
		"References: <root@x> <orig@x>", "Message-Id: <" + id + ">"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	if !strings.HasSuffix(id, "@example.org") || strings.Contains(id, "localhost") {
		t.Errorf("Message-ID %q", id)
	}
	h, err := Parse(raw)
	if err != nil || h.Subject != "Réunion demain" || len(h.To) != 1 || len(h.Cc) != 1 || h.InReplyTo != "orig@x" {
		t.Errorf("parse back = %+v, %v", h, err)
	}
	c, _ := Bodies(raw)
	if !strings.Contains(c.Plain, "> quoted line") || strings.Count(c.Plain, "word") != 40 {
		t.Errorf("body = %q", c.Plain)
	}
}

func TestBuildFlowedLines(t *testing.T) {
	raw, _, err := Build(Outgoing{From: "me@example.org", To: []string{"a@example.org"}, Subject: "s",
		Body: strings.Repeat("abcdefghi ", 20) + "\n"}, time.Now(), fixedRand())
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw[bytes.Index(raw, []byte("\r\n\r\n"))+4:])
	for _, line := range strings.Split(strings.TrimRight(body, "\r\n"), "\r\n") {
		if len(line) > 78 {
			t.Errorf("line longer than 78: %q", line)
		}
	}
	if !strings.Contains(body, "abcdefghi \r\n") { // a soft break keeps its trailing space
		t.Errorf("no flowed soft breaks:\n%s", body)
	}
}

func TestBuildAttachmentsAndBcc(t *testing.T) {
	raw, _, err := Build(Outgoing{From: "me@example.org", To: []string{"a@example.org"}, Bcc: []string{"b@example.org"},
		Subject: "s", Body: "see attached\n", IncludeBcc: true,
		Attachments: []OutgoingAttachment{{Filename: "../../etc/report.pdf", ContentType: "application/pdf", Data: []byte("%PDF")}}},
		time.Now(), fixedRand())
	if err != nil {
		t.Fatal(err)
	}
	c, _ := Bodies(raw)
	if len(c.Attachments) != 1 || c.Attachments[0].Filename != "report.pdf" || !strings.Contains(string(raw), "Bcc: <b@example.org>") ||
		!strings.Contains(c.Plain, "see attached") {
		t.Errorf("attachments %+v, plain %q; bcc kept in the stored copy?\n%s", c.Attachments, c.Plain, raw)
	}
}

func TestBuildRejectsHeaderInjection(t *testing.T) {
	if _, _, err := Build(Outgoing{From: "me@example.org", To: []string{"a@example.org"}, Subject: "x\r\nBcc: evil@x"},
		time.Now(), fixedRand()); err == nil {
		t.Error("CR/LF in a header value was accepted")
	}
	if _, _, err := Build(Outgoing{From: "not an address", To: []string{"a@example.org"}, Subject: "x"},
		time.Now(), fixedRand()); err == nil {
		t.Error("a bad From was accepted")
	}
}

func TestParseSeparatesRecipients(t *testing.T) {
	h, err := Parse([]byte("From: a@x.org\r\nTo: to@x.org\r\nCc: cc@x.org\r\nReply-To: r@x.org\r\nSubject: s\r\n\r\nb"))
	if err != nil || len(h.To) != 1 || h.To[0] != "to@x.org" || len(h.Cc) != 1 || h.Cc[0] != "cc@x.org" ||
		len(h.ReplyTo) != 1 || h.ReplyTo[0] != "r@x.org" {
		t.Errorf("parsed = %+v, %v", h, err)
	}
}

func FuzzBuild(f *testing.F) {
	f.Add("Hello", "body text\n> quote\n", "Ana <ana@example.org>")
	f.Fuzz(func(t *testing.T, subject, body, to string) {
		raw, _, err := Build(Outgoing{From: "me@example.org", To: []string{to}, Subject: subject, Body: body},
			time.Unix(0, 0), fixedRand())
		if err != nil {
			return
		}
		if _, err := Parse(raw); err != nil {
			t.Fatalf("built message does not parse: %v\n%s", err, raw)
		}
		_, _ = Bodies(raw)
	})
}
