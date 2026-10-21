// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"errors"
	"slices"
	"testing"

	"github.com/pluja/nought/internal/index"
)

// ref names a message found by a query, for the Messages inputs.
func ref(t *testing.T, env *Env, q string) MessageRef {
	t.Helper()
	return MessageRef{ID: idOf(t, env, q), Account: "personal"}
}

// Actions on a list of named messages (batch actions): exactly those, the
// trash included as for a bare id, in one go; all are resolved before any
// changes, so one unknown id changes nothing.
func TestTagNamedMessages(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	one, two := ref(t, env, `subject:"read one"`), ref(t, env, `subject:"unread two"`)
	if _, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Query: `subject:"read one"`}); err != nil {
		t.Fatal(err)
	}

	in := TagIn{Messages: []MessageRef{one, two, two}, Add: []string{"batch"}}
	if out, err := Call(t.Context(), env, Tag, MsgTag, TagIn{Messages: in.Messages, Add: in.Add, DryRun: true}); err != nil || out.Changed != 2 || !out.DryRun {
		t.Errorf("dry run = %+v, %v", out, err)
	}
	if got := slices.Concat(search(t, env, "tag:batch"), search(t, env, "tag:batch tag:deleted")); len(got) != 0 {
		t.Errorf("the dry run tagged %v", got)
	}
	out, err := Call(t.Context(), env, Tag, MsgTag, in)
	if err != nil || out.Matched != 2 || out.Changed != 2 {
		t.Fatalf("tag = %+v, %v", out, err)
	}
	if got := search(t, env, "tag:batch tag:deleted"); !slices.Equal(got, []string{"read one"}) {
		t.Errorf("the trashed message: %v", got)
	}
	if got := search(t, env, "tag:batch"); len(got) != 2 { // search() sees the trash too
		t.Errorf("tagged %v", got)
	}

	for name, bad := range map[string]TagIn{
		"query and messages":   {Query: "tag:inbox", Messages: []MessageRef{two}, Add: []string{"x"}},
		"account and messages": {Account: "personal", Messages: []MessageRef{two}, Add: []string{"x"}},
		"too many":             {Messages: make([]MessageRef, index.MaxLimit+1), Add: []string{"x"}},
	} {
		if _, err := Call(t.Context(), env, Tag, MsgTag, bad); !errors.Is(err, ErrUsage) {
			t.Errorf("%s: %v", name, err)
		}
	}
	missing := MessageRef{ID: "aaaaaaaaaaaa", Account: "personal"}
	if _, err := Call(t.Context(), env, Tag, MsgTag, TagIn{Messages: []MessageRef{two, missing}, Add: []string{"x"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
	if got := search(t, env, "tag:x"); len(got) != 0 {
		t.Errorf("tagged %v although one id was unknown", got)
	}
}

func TestDeleteNamedMessages(t *testing.T) {
	env := newTestEnv(t)
	importSample(t, env)
	one, two := ref(t, env, `subject:"read one"`), ref(t, env, `subject:"unread two"`)
	out, err := Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Messages: []MessageRef{one}})
	if err != nil || out.Matched != 1 {
		t.Fatalf("delete = %+v, %v", out, err)
	}
	if got := search(t, env, "tag:deleted"); !slices.Equal(got, []string{"read one"}) {
		t.Errorf("trash = %v", got)
	}
	out, err = Call(t.Context(), env, Delete, MsgDelete, DeleteIn{Messages: []MessageRef{one, two}, Now: true})
	if err != nil || out.Matched != 2 || out.Purged != 2 {
		t.Fatalf("delete now = %+v, %v", out, err)
	}
	if got := search(t, env, "* OR tag:deleted"); slices.Contains(got, "read one") || slices.Contains(got, "unread two") {
		t.Errorf("still here: %v", got)
	}
}
