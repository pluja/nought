// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mailmsg extracts what nought needs from raw RFC 5322 messages:
// header fields for the index, and the text parts for search and display.
// All input is treated as hostile; parsing is best effort and never panics.
package mailmsg

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	To         []string // To only
	Cc         []string
	ReplyTo    []string
	Subject    string
	References []string
	InReplyTo  string
}

// Limits keep hostile messages from using unbounded memory or time, and
// keep index rows reasonable.
const (
	maxParts     = 1000
	maxTextPart  = 4 << 20 // bytes of decoded text kept per part
	maxField     = 2000    // runes kept of a header field such as Subject
	maxAddresses = 200     // To + Cc addresses kept
	maxMsgID     = 998     // RFC 5322 line length; longer IDs are ignored
	// go-message keeps a header field in about a hundred times its size
	// when fields are tiny: a hostile header of a few MiB took gigabytes.
	// Real mail has dozens of fields; the rest of the header is dropped.
	maxHeaderFields = 1000
	maxFieldBytes   = 1 << 20 // DKIM and ARC fields, the longest in real mail, take kilobytes
	maxHeaderBytes  = 8 << 20
	maxListBytes    = 64 << 10 // of an address or Message-ID list parsed
	maxReferences   = 50       // the first and the last ones (RFC 5322 3.6.4)
)

// readOptions lifts go-message's 1 MiB header limit: the raw message is
// already in memory and size-limited by the caller, and a single huge header
// must not hide every other field.
var readOptions = &message.ReadOptions{MaxHeaderBytes: -1}

// Parse reads the header of raw. It returns whatever it could parse; a
// non-nil error only explains what was skipped.
func Parse(raw []byte) (Header, error) {
	e, err := read(raw)
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
	out.To = limit(addresses(h, "To"))
	out.Cc = limit(addresses(h, "Cc"))
	out.ReplyTo = limit(addresses(h, "Reply-To"))
	out.References = firstAndLast(msgIDs(h, "References"), maxReferences)
	if ids := msgIDs(h, "In-Reply-To"); len(ids) > 0 {
		out.InReplyTo = ids[0]
	}
	return out, errors.Join(errs...)
}

func limit(list []string) []string { return list[:min(len(list), maxAddresses)] }

// text returns a decoded header value, falling back to the raw value.
func text(h mail.Header, key string) string {
	if v, err := h.Text(key); err == nil {
		return oneLine(v)
	}
	return oneLine(h.Get(key))
}

func addresses(h mail.Header, key string) []string {
	v := h.Get(key)
	if len(v) > maxListBytes { // cut after the last whole address that fits
		v = v[:max(strings.LastIndexByte(v[:maxListBytes], ','), 0)]
	}
	list, err := mail.ParseAddressList(v)
	if err != nil || len(list) == 0 {
		var cut mail.Header
		cut.Set(key, v)
		if v := text(cut, key); v != "" {
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

func msgIDs(full mail.Header, key string) []string {
	var h mail.Header
	h.Set(key, headAndTail(full.Get(key), maxListBytes))
	ids, err := h.MsgIDList(key)
	if err != nil {
		// Be lenient, but only with tokens that look like IDs, so prose
		// such as "see my previous mail" does not become a thread key.
		ids = nil
		for f := range strings.FieldsSeq(h.Get(key)) {
			if strings.Contains(f, "@") || strings.HasPrefix(f, "<") && strings.HasSuffix(f, ">") {
				ids = append(ids, f)
			}
		}
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
// otherwise compared byte for byte, as notmuch does. IDs that are too long or
// contain control characters are treated as missing.
func NormalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "<")
	id = strings.TrimSuffix(id, ">")
	id = strings.TrimSpace(id)
	if len(id) > maxMsgID || strings.IndexFunc(id, unicode.IsControl) >= 0 || !utf8.ValidString(id) {
		return ""
	}
	return id
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
	e, err := read(raw)
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
			boundParts(part)
			return nil
		}
		mediaType, filename, isAttachment := partKind(part)
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

// partKind classifies a leaf part: its media type, its file name, and
// whether it is an attachment (rather than the text of the message).
func partKind(part *message.Entity) (mediaType, filename string, attachment bool) {
	mediaType, ctParams, _ := part.Header.ContentType()
	if mediaType == "" {
		mediaType = "text/plain"
	}
	disp, params, _ := part.Header.ContentDisposition()
	filename = params["filename"]
	if filename == "" {
		filename = ctParams["name"]
	}
	filename = decodeWord(filename)
	attachment = disp == "attachment" || filename != "" || !strings.HasPrefix(mediaType, "text/")
	return mediaType, filename, attachment
}

// maxAttachmentBytes bounds what AttachmentData reads.
const maxAttachmentBytes = 256 << 20

// AttachmentData returns a message's attachments with their contents (to
// forward them).
func AttachmentData(raw []byte) ([]OutgoingAttachment, error) {
	e, err := read(raw)
	if e == nil {
		return nil, fmt.Errorf("parse message: %w", err)
	}
	var out []OutgoingAttachment
	var total int64
	parts := 0
	err = e.Walk(func(_ []int, part *message.Entity, err error) error {
		if parts++; parts > maxParts {
			return errTooManyParts
		}
		if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
			return err
		}
		if part.MultipartReader() != nil {
			boundParts(part)
			return nil
		}
		mediaType, filename, attachment := partKind(part)
		if !attachment {
			return nil
		}
		data, err := io.ReadAll(io.LimitReader(part.Body, maxAttachmentBytes-total+1))
		if err != nil {
			return err
		}
		if total += int64(len(data)); total > maxAttachmentBytes {
			return errors.New("attachments are too large")
		}
		out = append(out, OutgoingAttachment{Filename: oneLine(filename), ContentType: mediaType, Data: data})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read attachments: %w", err)
	}
	return out, nil
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

// oneLine collapses folded header values, strips invalid UTF-8, and caps
// the length.
func oneLine(s string) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxField {
		s = string([]rune(s)[:maxField])
	}
	return s
}

// headAndTail keeps the start and the end of a long list of
// space-separated items, at most limit bytes, cut between items.
func headAndTail(v string, limit int) string {
	if len(v) <= limit {
		return v
	}
	head := v[:limit/2]
	if i := strings.LastIndexByte(head, ' '); i >= 0 {
		head = head[:i]
	}
	tail := v[len(v)-limit/2:]
	if i := strings.IndexByte(tail, ' '); i >= 0 {
		tail = tail[i:]
	}
	return head + tail
}

// firstAndLast keeps the first item and the last n-1 of a long list.
func firstAndLast(list []string, n int) []string {
	if len(list) <= n {
		return list
	}
	return append(list[:1:1], list[len(list)-(n-1):]...)
}
