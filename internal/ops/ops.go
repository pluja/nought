// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ops holds every operation nought can perform. Frontends (CLI, MCP,
// TUI, GUI) are thin adapters: they parse input, call one op through Call,
// and render its output. Permission checks happen here and only here.
package ops

import (
	"context"
	"errors"
	"fmt"
)

// Permission orders what a caller may do. Each level includes the ones below.
type Permission int

// Permission levels, from least to most powerful.
const (
	Read Permission = iota + 1
	Tag
	Draft
	Send
	Delete
)

// ParsePermission reads a permission by its name (read, tag, draft, send,
// delete).
func ParsePermission(s string) (Permission, error) {
	for _, p := range []Permission{Read, Tag, Draft, Send, Delete} {
		if s == p.String() {
			return p, nil
		}
	}
	return Read, usageError("%q is not a permission: use read, tag, draft, send or delete", s)
}

func (p Permission) String() string {
	switch p {
	case Read:
		return "read"
	case Tag:
		return "tag"
	case Draft:
		return "draft"
	case Send:
		return "send"
	case Delete:
		return "delete"
	default:
		return fmt.Sprintf("permission(%d)", int(p))
	}
}

// Op is one operation. Its In and Out types are plain structs; Out's JSON
// encoding is the operation's stable machine-readable output. Ops are
// declared as package-level variables so frontends can list them; they are
// definitions and are never reassigned.
type Op[In, Out any] struct {
	Name    string // dotted name, e.g. "msg.tag"; also the future MCP tool name
	Summary string
	Perm    Permission
	// PermFor, if set, may raise the level for a given input (tagging is
	// a tag-level action, unless the tags schedule a deletion).
	PermFor func(in In) Permission
	Run     func(ctx context.Context, env *Env, in In) (Out, error)
}

// Call runs op on behalf of a caller holding the given permission.
func Call[In, Out any](ctx context.Context, env *Env, caller Permission, op Op[In, Out], in In) (Out, error) {
	need := op.Perm
	if op.PermFor != nil {
		need = max(need, op.PermFor(in))
	}
	if caller < need {
		var zero Out
		return zero, fmt.Errorf("%s requires %s permission, caller has %s: %w", op.Name, need, caller, ErrPermission)
	}
	return op.Run(ctx, env, in)
}

// Error kinds that frontends map to exit codes or protocol errors. Ops wrap
// them with context; callers test with errors.Is.
var (
	ErrUsage      = errors.New("usage error")
	ErrNotFound   = errors.New("not found")
	ErrAmbiguous  = errors.New("ambiguous")
	ErrPermission = errors.New("permission denied")
	ErrPartial    = errors.New("partial failure")
	ErrBusy       = errors.New("busy")
)

func usageError(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrUsage)
}
