// SPDX-License-Identifier: AGPL-3.0-or-later

package index

import (
	"bytes"
	"strings"
	"testing"
)

func TestCheckRestoreChangesNothing(t *testing.T) {
	ix, _ := openIndex(t)
	sample(t, ix)
	var before bytes.Buffer
	if err := ix.Dump(t.Context(), &before, nil); err != nil {
		t.Fatal(err)
	}
	res, err := ix.CheckRestore(t.Context(), strings.NewReader("+work -- id:one@x\n+x -- id:nobody@x\n"))
	if err != nil || res.Applied != 1 || res.Missing != 1 {
		t.Fatalf("check = %+v, %v", res, err)
	}
	var after bytes.Buffer
	if err := ix.Dump(t.Context(), &after, nil); err != nil {
		t.Fatal(err)
	}
	if after.String() != before.String() {
		t.Errorf("a check changed tags:\n%s", after.String())
	}
	if _, err := ix.CheckRestore(t.Context(), strings.NewReader("+x no separator\n")); err == nil {
		t.Error("a bad line passed the check")
	}
}
