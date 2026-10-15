// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mailmsg extracts what nought needs from raw RFC 5322 messages:
// header fields for the index, and the text parts for search and display.
// All input is treated as hostile; parsing is best effort and never panics.
package mailmsg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // decode legacy charsets (ISO-8859-*, Windows-125x, ...)
	"github.com/emersion/go-message/mail"
)

// Header holds the fields nought indexes. Missing or malformed fields are
// left empty rather than failing the whole message.
type Header struct {
	MessageID  string
	Date       time.Time
	From       string
	To         []string // To and Cc
	Subject    string
	References []string
	InReplyTo  string
}

// Limits keep hostile messages from using unbounded memory or time.
const (
	maxParts    = 1000
	maxTextPart = 4 << 20 // bytes of decoded text kept per part
)

// Parse reads the header of raw. It returns whatever it could parse; a
// non-nil error only explains what was skipped.
func Parse(raw []byte) (Header, error) {
	e, err := message.Read(bytes.NewReader(raw))
	if e == nil {
		return Header{}, fmt.Errorf("parse header: %w", err)
	}
	h := mail.Header{Header: e.Header}
	var out Header
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}

	if id, err := h.MessageID(); err == nil {
		out.MessageID = NormalizeMessageID(id)
	} else {
		// Fall back to the raw value for IDs that violate the grammar.
		out.MessageID = NormalizeMessageID(h.Get("Message-Id"))
	}
	if d, err := h.Date(); err == nil {
		out.Date = d.UTC()
	} else if h.Get("Date") != "" {
		errs = append(errs, fmt.Errorf("date: %w", err))
	}
	out.Subject = text(h, "Subject")
	out.From = firstAddress(h, "From")
	out.To = append(addresses(h, "To"), addresses(h, "Cc")...)
	out.References = msgIDs(h, "References")
	if ids := msgIDs(h, "In-Reply-To"); len(ids) > 0 {
		out.InReplyTo = ids[0]
	}
	return out, errors.Join(errs...)
}

// text returns a decoded header value, falling back to the raw value.
func text(h mail.Header, key string) string {
	if v, err := h.Text(key); err == nil {
		return oneLine(v)
	}
	return oneLine(h.Get(key))
}

func addresses(h mail.Header, key string) []string {
	list, err := h.AddressList(key)
	if err != nil || len(list) == 0 {
		if v := text(h, key); v != "" {
			return []string{v}
		}
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, formatAddress(a))
	}
	return out
}

func firstAddress(h mail.Header, key string) string {
	if list := addresses(h, key); len(list) > 0 {
		return list[0]
	}
	return ""
}

func formatAddress(a *mail.Address) string {
	if a.Name == "" {
		return a.Address
	}
	return oneLine(a.Name) + " <" + a.Address + ">"
}

func msgIDs(h mail.Header, key string) []string {
	ids, err := h.MsgIDList(key)
	if err != nil {
		// Be lenient: split on whitespace and strip brackets.
		ids = strings.Fields(h.Get(key))
	}
	var out []string
	for _, id := range ids {
		if id = NormalizeMessageID(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// NormalizeMessageID trims whitespace and angle brackets. Message-IDs are
// otherwise compared byte for byte, as notmuch does.
func NormalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	return strings.TrimSpace(id)
}

// ThreadKey groups a message with its conversation: the root of its
// References, else its parent, else itself.
func ThreadKey(h Header, hash string) string {
	switch {
	case len(h.References) > 0:
		return h.References[0]
	case h.InReplyTo != "":
		return h.InReplyTo
	case h.MessageID != "":
		return h.MessageID
	default:
		return "hash:" + hash
	}
}

// Attachment describes a non-text part.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// Content is the readable content of a message.
type Content struct {
	Plain       string
	HTML        string
	Attachments []Attachment
}

var errTooManyParts = errors.New("too many MIME parts")

// Bodies returns the first text/plain and text/html parts that are not
// attachments, decoded to UTF-8, plus a list of attachments.
func Bodies(raw []byte) (Content, error) {
	var c Content
	e, err := message.Read(bytes.NewReader(raw))
	if e == nil {
		return c, fmt.Errorf("parse message: %w", err)
	}
	parts := 0
	walkErr := e.Walk(func(_ []int, part *message.Entity, err error) error {
		if parts++; parts > maxParts {
			return errTooManyParts
		}
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			return err
		}
		if part.MultipartReader() != nil {
			return nil
		}
		mediaType, _, _ := part.Header.ContentType()
		if mediaType == "" {
			mediaType = "text/plain"
		}
		disp, params, _ := part.Header.ContentDisposition()
		filename := params["filename"]
		if filename == "" {
			_, ctParams, _ := part.Header.ContentType()
			filename = ctParams["name"]
		}
		filename = decodeWord(filename)
		isAttachment := disp == "attachment" || filename != ""

		switch {
		case !isAttachment && mediaType == "text/plain" && c.Plain == "":
			c.Plain = readText(part.Body)
		case !isAttachment && mediaType == "text/html" && c.HTML == "":
			c.HTML = readText(part.Body)
		case isAttachment || !strings.HasPrefix(mediaType, "text/"):
			n, _ := io.Copy(io.Discard, part.Body)
			c.Attachments = append(c.Attachments, Attachment{Filename: oneLine(filename), ContentType: mediaType, Size: n})
		}
		return nil
	})
	if errors.Is(walkErr, errTooManyParts) {
		return c, walkErr
	}
	if walkErr != nil {
		return c, fmt.Errorf("read parts: %w", walkErr)
	}
	return c, nil
}

func readText(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, maxTextPart))
	return strings.ToValidUTF8(string(b), "�")
}

var wordDecoder = mime.WordDecoder{CharsetReader: message.CharsetReader}

func decodeWord(s string) string {
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		return d
	}
	return s
}

// oneLine collapses folded header values and strips invalid UTF-8.
func oneLine(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.Join(strings.Fields(s), " ")
}
