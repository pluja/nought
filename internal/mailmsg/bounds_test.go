// SPDX-License-Identifier: AGPL-3.0-or-later

package mailmsg

import (
	"bytes"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/emersion/go-message"
)

// allocated is how many bytes f allocates.
func allocated(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// Review: go-message keeps header fields at about 160 bytes per byte when
// they are tiny, so one hostile header of a few MiB took gigabytes, at
// every sync and every read. Parsing stays within a small multiple of the
// message's size.
func TestHostileHeadersAreBounded(t *testing.T) {
	const size = 4 << 20
	for name, raw := range map[string][]byte{
		"many fields":      append(bytes.Repeat([]byte("a:\n"), size/3), "\nbody\n"...),
		"one huge To":      []byte("Subject: s\nTo: " + strings.Repeat("a@b.c, ", size/7) + "\n\nbody\n"),
		"huge References":  []byte("Subject: s\nReferences: " + strings.Repeat("<a@b> ", size/6) + "\n\nbody\n"),
		"huge field first": []byte("X-Junk: " + strings.Repeat("x", size) + "\nSubject: s\n\nbody\n"),
		// Review: part headers were not bounded at all.
		"part header": []byte("Content-Type: multipart/mixed; boundary=b\n\n--b\n" +
			strings.Repeat("a:b\n", size/4) + "\nhello\n--b--\n"),
		"nested part header": []byte("Content-Type: multipart/mixed; boundary=o\n\n--o\n" +
			"Content-Type: multipart/alternative; boundary=i\n\n--i\n" +
			strings.Repeat("a:b\n", size/4) + "\nhello\n--i--\n--o--\n"),
		"long list fits": []byte("Subject: s\nTo: " + strings.Repeat("a@b.c, ", 100<<10/7) + "\nReferences: <first@x> " + strings.Repeat("<a@b> ", 100<<10/6) + "<last@x>\n\nbody\n"),
	} {
		t.Run(name, func(t *testing.T) {
			var h Header
			n := allocated(func() {
				h, _ = Parse(raw)
				_, _ = Bodies(raw)
				_, _ = AttachmentData(raw)
			})
			t.Logf("%d MiB allocated for a %d MiB message", n>>20, len(raw)>>20)
			if n > 8*uint64(len(raw))+(16<<20) {
				t.Errorf("%d MiB allocated for a %d MiB message", n>>20, len(raw)>>20)
			}
			if len(h.To) > maxAddresses || len(h.References) > maxReferences {
				t.Errorf("kept %d addresses, %d references", len(h.To), len(h.References))
			}
			if strings.Contains(name, "part header") {
				if c, _ := Bodies(raw); strings.TrimSpace(c.Plain) != "hello" {
					t.Errorf("text of the part = %q", c.Plain)
				}
				return
			}
			// A huge field hides nothing else.
			if h.Subject != "s" && name != "many fields" {
				t.Errorf("subject %q lost", h.Subject)
			}
			if name == "long list fits" && (len(h.To) != maxAddresses || len(h.References) != maxReferences ||
				h.References[0] != "first@x" || h.References[len(h.References)-1] != "last@x") {
				t.Errorf("to %d, references %d (%v … %v)", len(h.To), len(h.References), h.References[:1], h.References[len(h.References)-1:])
			}
		})
	}
}

// Mail within the bounds passes the part filter exactly as it is.
func FuzzPartHeadersPassNormalMail(f *testing.F) {
	f.Add([]byte("--b\nContent-Type: text/plain\n\nhi\n--b--\n"))
	f.Add([]byte("preamble\r\n--b\r\nA: 1\r\n folded\r\n\r\nbody\r\n--b \r\nB: 2\r\n\r\n--b--\r\nepilogue"))
	f.Fuzz(func(t *testing.T, body []byte) {
		e, err := message.Read(bytes.NewReader(append([]byte("Content-Type: multipart/mixed; boundary=b\n\n"), body...)))
		if e == nil {
			t.Skip(err)
		}
		boundParts(e)
		got, err := io.ReadAll(e.Body)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("changed: %q -> %q (%v)", body, got, err)
		}
	})
}
