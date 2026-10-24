// SPDX-License-Identifier: AGPL-3.0-or-later

package mailmsg

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
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
		"long list fits":   []byte("Subject: s\nTo: " + strings.Repeat("a@b.c, ", 100<<10/7) + "\nReferences: <first@x> " + strings.Repeat("<a@b> ", 100<<10/6) + "<last@x>\n\nbody\n"),
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
