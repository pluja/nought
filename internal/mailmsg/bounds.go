// SPDX-License-Identifier: AGPL-3.0-or-later

package mailmsg

import (
	"bufio"
	"bytes"
	"errors"

	"github.com/emersion/go-message"
)

// Bounds on headers, before go-message parses them: it keeps a header
// field at about a hundred times its size when fields are tiny, so a
// hostile header of a few MiB took gigabytes, at every sync and read.

// read parses raw, with its header bounded to maxHeaderFields fields.
func read(raw []byte) (*message.Entity, error) {
	return message.ReadWithOptions(bytes.NewReader(boundHeader(raw)), readOptions)
}

// boundHeader returns raw with a bounded header: at most maxHeaderFields
// fields and maxHeaderBytes in all, none over maxFieldBytes. A field over
// the limits is dropped, not the fields after it, so one huge field hides
// nothing else. The body is kept as it is; most messages come back as
// they are.
func boundHeader(raw []byte) []byte {
	type span struct{ start, end int }
	var keep []span
	total, dropped := 0, false
	field := span{-1, -1}
	closeField := func(end int) {
		if field.start < 0 {
			return
		}
		n := end - field.start
		if len(keep) < maxHeaderFields && n <= maxFieldBytes && total+n <= maxHeaderBytes {
			keep = append(keep, span{field.start, end})
			total += n
		} else {
			dropped = true
		}
		field.start = -1
	}
	rest := len(raw) // where the body (with the blank line before it) starts
	for i := 0; i < len(raw); {
		end := len(raw)
		if j := bytes.IndexByte(raw[i:], '\n'); j >= 0 {
			end = i + j + 1
		}
		line := bytes.TrimRight(raw[i:end], "\r\n")
		if len(line) == 0 {
			rest = i
			break
		}
		if line[0] != ' ' && line[0] != '\t' || field.start < 0 {
			closeField(i)
			field.start = i
		}
		i = end
	}
	closeField(rest)
	if !dropped {
		return raw
	}
	out := make([]byte, 0, total+len(raw)-rest)
	for _, f := range keep {
		out = append(out, raw[f.start:f.end]...)
	}
	return append(out, raw[rest:]...)
}

// boundParts bounds the headers of a multipart entity's parts, as
// boundHeader does a message's: go-message reads part headers with no
// limit at all. It is called on each multipart entity before its parts
// are read, so nested ones are bounded in turn.
func boundParts(e *message.Entity) {
	_, params, _ := e.Header.ContentType()
	if b := params["boundary"]; b != "" {
		delim := []byte("--" + b)
		e.Body = &partHeaders{r: bufio.NewReaderSize(e.Body, lineBuffer), delim: delim,
			closing: append(bytes.Clone(delim), '-', '-')}
	}
}

// lineBuffer is the longest header line kept: lines are folded at 998
// characters in mail that follows the standard.
const lineBuffer = 64 << 10

// partHeaders passes a multipart body on, line by line, dropping from each
// part's header the lines over the bounds. Whole lines are kept or
// dropped, so no field runs into the next.
type partHeaders struct {
	r              *bufio.Reader
	delim, closing []byte
	header         bool // between a part's delimiter line and the blank line after it
	field          bool // the header field being read is kept
	keep, midLine  bool // the line being read is kept; it goes on in the next chunk
	fields, total  int  // fields and bytes kept of this part's header
	fieldN         int  // bytes kept of the current field
	out            []byte
	err            error
}

func (p *partHeaders) Read(b []byte) (int, error) {
	for len(p.out) == 0 {
		if p.err != nil {
			return 0, p.err
		}
		chunk, err := p.r.ReadSlice('\n')
		partial := errors.Is(err, bufio.ErrBufferFull)
		if !partial {
			p.err = err
		}
		if p.line(chunk, partial) {
			p.out = chunk
		}
	}
	n := copy(b, p.out)
	p.out = p.out[n:]
	return n, nil
}

// line reports whether a chunk of input is passed on. partial: the line
// goes on in the next chunk, which follows the decision made here.
func (p *partHeaders) line(chunk []byte, partial bool) bool {
	if p.midLine {
		p.midLine = partial
		return p.keep
	}
	p.midLine = partial
	text := bytes.TrimRight(chunk, " \t\r\n")
	switch {
	case bytes.Equal(text, p.delim):
		p.header, p.fields, p.total, p.keep = true, 0, 0, true
	case bytes.Equal(text, p.closing): // the end of the parts
		p.header, p.keep = false, true
	case !p.header: // a part's body, or the text around the parts
		p.keep = true
	case len(text) == 0 && !partial: // the end of this part's header
		p.header, p.keep = false, true
	case (chunk[0] == ' ' || chunk[0] == '\t') && p.fields > 0: // a folded line of the field
		p.keep = p.field && !partial && p.fits(len(chunk))
		p.field = p.keep
	default: // a new field (or a header's stray first folded line, as one)
		p.fields++
		p.fieldN = 0
		p.keep = p.fields <= maxHeaderFields && !partial && p.fits(len(chunk))
		p.field = p.keep
	}
	return p.keep
}

// fits counts n more header bytes, if the field and the header can take them.
func (p *partHeaders) fits(n int) bool {
	if p.fieldN+n > maxFieldBytes || p.total+n > maxHeaderBytes {
		return false
	}
	p.fieldN += n
	p.total += n
	return true
}
