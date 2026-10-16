// SPDX-License-Identifier: AGPL-3.0-or-later

package mailmsg

import (
	"bytes"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-message/mail"
)

// Outgoing is a message to send.
type Outgoing struct {
	From        string   // "Name <addr>" or "addr"
	To, Cc      []string // the same forms
	Bcc         []string // written only with IncludeBcc
	Subject     string
	InReplyTo   string   // a bare Message-ID, without <>
	References  []string // bare Message-IDs
	Body        string   // plain text, "\n" line ends; sent as format=flowed
	Attachments []OutgoingAttachment
	// IncludeBcc writes the Bcc header: for the copy kept locally, never for
	// the bytes sent.
	IncludeBcc bool
}

// OutgoingAttachment is a file to attach.
type OutgoingAttachment struct {
	Filename    string // only its base name is written
	ContentType string
	Data        []byte
}

// flowedWidth is where format=flowed lines are broken.
const flowedWidth = 72

// Build writes an outgoing message and returns it with its Message-ID. All
// outgoing privacy rules live here: the date in UTC, a random Message-ID on
// the sender's domain (no hostname), no client fingerprint headers,
// format=flowed plain text, attachments named by base name only, and Bcc
// only in the stored copy. now and random are injected for tests.
func Build(m Outgoing, now time.Time, random io.Reader) ([]byte, string, error) {
	values := append([]string{m.From, m.Subject, m.InReplyTo}, m.References...)
	for _, list := range [][]string{m.To, m.Cc, m.Bcc} {
		values = append(values, list...)
	}
	for _, v := range values {
		if strings.ContainsAny(v, "\r\n") {
			return nil, "", errors.New("a header value contains a line break")
		}
	}
	from, err := mail.ParseAddress(m.From)
	if err != nil {
		return nil, "", fmt.Errorf("from %q: %w", m.From, err)
	}
	id, err := messageID(from.Address, random)
	if err != nil {
		return nil, "", err
	}
	var h mail.Header
	h.SetDate(now.UTC())
	h.SetMessageID(id)
	h.SetAddressList("From", []*mail.Address{from})
	lists := []struct {
		key  string
		list []string
	}{{"To", m.To}, {"Cc", m.Cc}}
	if m.IncludeBcc {
		lists = append(lists, struct {
			key  string
			list []string
		}{"Bcc", m.Bcc})
	}
	for _, l := range lists {
		if err := setList(&h, l.key, l.list); err != nil {
			return nil, "", err
		}
	}
	h.SetSubject(m.Subject)
	if m.InReplyTo != "" {
		h.SetMsgIDList("In-Reply-To", []string{m.InReplyTo})
	}
	if len(m.References) > 0 {
		h.SetMsgIDList("References", m.References)
	}
	var buf bytes.Buffer
	if err := writeBody(&buf, h, m); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), id, nil
}

func setList(h *mail.Header, key string, list []string) error {
	if len(list) == 0 {
		return nil
	}
	addrs := make([]*mail.Address, 0, len(list))
	for _, s := range list {
		a, err := mail.ParseAddress(s)
		if err != nil {
			return fmt.Errorf("%s %q: %w", strings.ToLower(key), s, err)
		}
		addrs = append(addrs, a)
	}
	h.SetAddressList(key, addrs)
	return nil
}

// messageID is 26 random characters at the sender's domain.
func messageID(address string, random io.Reader) (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", fmt.Errorf("message id: %w", err)
	}
	domain := address[strings.LastIndexByte(address, '@')+1:]
	local := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:26]
	return local + "@" + domain, nil
}

func writeBody(w io.Writer, h mail.Header, m Outgoing) error {
	text := flowed(m.Body)
	var inline mail.InlineHeader
	inline.SetContentType("text/plain", map[string]string{"charset": "utf-8", "format": "flowed"})
	inline.Set("Content-Transfer-Encoding", transferEncoding(text))
	if len(m.Attachments) == 0 {
		h.SetContentType("text/plain", map[string]string{"charset": "utf-8", "format": "flowed"})
		h.Set("Content-Transfer-Encoding", transferEncoding(text))
		pw, err := mail.CreateSingleInlineWriter(w, h)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(pw, text); err != nil {
			return err
		}
		return pw.Close()
	}
	mw, err := mail.CreateWriter(w, h)
	if err != nil {
		return err
	}
	pw, err := mw.CreateSingleInline(inline)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(pw, text); err != nil {
		return err
	}
	if err := pw.Close(); err != nil {
		return err
	}
	for _, a := range m.Attachments {
		var ah mail.AttachmentHeader
		// Parameters are dropped: Go gives "text/plain; charset=utf-8"
		// for .txt, which go-message would otherwise write as an empty
		// type. Attachments travel in base64, so a charset changes nothing.
		ct, _, err := mime.ParseMediaType(a.ContentType)
		if err != nil {
			ct = "application/octet-stream"
		}
		ah.SetContentType(ct, nil)
		ah.Set("Content-Transfer-Encoding", "base64")
		ah.SetFilename(filepath.Base(filepath.Clean("/" + filepath.ToSlash(a.Filename))))
		aw, err := mw.CreateAttachment(ah)
		if err != nil {
			return err
		}
		if _, err := aw.Write(a.Data); err != nil {
			return err
		}
		if err := aw.Close(); err != nil {
			return err
		}
	}
	return mw.Close()
}

// transferEncoding is 7bit for plain ASCII text with short lines, else
// quoted-printable.
func transferEncoding(text string) string {
	for _, line := range strings.Split(text, "\r\n") {
		if len(line) > 998 {
			return "quoted-printable"
		}
	}
	for i := range len(text) {
		if text[i] >= utf8.RuneSelf {
			return "quoted-printable"
		}
	}
	return "7bit"
}

// flowed writes text as RFC 3676 format=flowed (DelSp=no), with CRLF line
// ends: long unquoted lines are broken at spaces before flowedWidth, each
// soft break keeping its trailing space; trailing spaces in the text are
// trimmed so they do not read as soft breaks (except the signature
// separator "-- "); lines starting with a space or "From " are
// space-stuffed. Quoted lines (">") are kept as they are.
// lines makes every line end in a bare LF, and drops control characters
// other than tab: a bare CR (say, in a quoted hostile message) must not
// reach the wire, where "<CR>.<CR>" may end the message early on some
// servers.
func lines(text string) string {
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text)
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}

func flowed(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(lines(text), "\n"), "\n") {
		if line != "-- " {
			line = strings.TrimRight(line, " ")
		}
		if strings.HasPrefix(line, ">") || line == "-- " {
			b.WriteString(line + "\r\n")
			continue
		}
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "From ") {
			line = " " + line
		}
		for len(line) > flowedWidth {
			cut := strings.LastIndexByte(line[:flowedWidth], ' ')
			if cut <= 0 {
				break // one long word: keep it whole
			}
			b.WriteString(line[:cut+1] + "\r\n")
			line = line[cut+1:]
			if strings.HasPrefix(line, " ") || strings.HasPrefix(line, ">") || strings.HasPrefix(line, "From ") {
				line = " " + line
			}
		}
		b.WriteString(line + "\r\n")
	}
	return b.String()
}
