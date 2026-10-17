// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/pluja/nought/internal/compose"
	"github.com/pluja/nought/internal/mailmsg"
)

// Drafts are local: an editor file (<id>.txt) and its details (<id>.json)
// under the drafts directory. They are never uploaded.

// Draft kinds.
const (
	KindNew      = "new"
	KindReply    = "reply"
	KindReplyAll = "reply-all"
	KindForward  = "forward"
)

// DraftInfo describes a draft.
type DraftInfo struct {
	ID         string    `json:"id"`
	Account    string    `json:"account"`
	Kind       string    `json:"kind"`
	Path       string    `json:"path"` // the editor file
	Subject    string    `json:"subject"`
	To         []string  `json:"to,omitempty"`
	SourceHash string    `json:"source_hash,omitempty"` // the message answered or forwarded
	InReplyTo  string    `json:"in_reply_to,omitempty"`
	References []string  `json:"references,omitempty"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
	LastError  string    `json:"last_error,omitempty"` // why it came back from the outbox
	// Untouched is the SHA-256 of the editor file as nought wrote it, until
	// the user changes it.
	Untouched string `json:"untouched,omitempty"`
}

// DraftOut is a draft with its content and the problems to look at.
type DraftOut struct {
	DraftInfo
	Draft    compose.Draft     `json:"draft"`
	Problems []compose.Problem `json:"problems"`
	// Discarded: the editor closed without anything written in a new
	// draft, so it was dropped.
	Discarded bool `json:"discarded,omitempty"`
	// Resumed: a draft written before, returned for DraftReplyIn.Resume.
	Resumed bool `json:"resumed,omitempty"`
}

// DraftNewIn starts a new message. Subject, Body and Attach are for
// scripts; the editor fills them otherwise.
type DraftNewIn struct {
	Account string   `json:"account,omitempty"`
	To      []string `json:"to,omitempty"`
	Cc      []string `json:"cc,omitempty"`
	Bcc     []string `json:"bcc,omitempty"`
	Subject string   `json:"subject,omitempty"`
	Body    string   `json:"body,omitempty"`
	Attach  []string `json:"attach,omitempty"`
}

// DraftReplyIn answers an archived message.
type DraftReplyIn struct {
	ID      string   `json:"id"`
	Account string   `json:"account,omitempty"`
	All     bool     `json:"all,omitempty"`
	Body    string   `json:"body,omitempty"` // written above the quote (scripts)
	Cc      []string `json:"cc,omitempty"`   // added to the copies
	Attach  []string `json:"attach,omitempty"`
	// Resume returns the draft already answering the message, if there is
	// one (not in the outbox), rather than start another from zero.
	Resume bool `json:"resume,omitempty"`
}

// DraftForwardIn forwards an archived message.
type DraftForwardIn struct {
	ID      string   `json:"id"`
	Account string   `json:"account,omitempty"`
	To      []string `json:"to,omitempty"`
	Cc      []string `json:"cc,omitempty"`
	Body    string   `json:"body,omitempty"`   // written above the forwarded message (scripts)
	Attach  []string `json:"attach,omitempty"` // added to the original's files
}

// DraftIDIn names a draft (or an outbox entry).
type DraftIDIn struct {
	ID string `json:"id"`
}

// DraftNew starts a new message.
var DraftNew = Op[DraftNewIn, DraftOut]{
	Name: "draft.new", Summary: "Start a new message (a local draft)", Perm: Draft,
	Run: func(ctx context.Context, env *Env, in DraftNewIn) (DraftOut, error) {
		if err := headerValues(slices.Concat(in.To, in.Cc, in.Bcc, in.Attach, []string{in.Subject})...); err != nil {
			return DraftOut{}, err
		}
		a, err := sendingAccount(env, in.Account)
		if err != nil {
			return DraftOut{}, err
		}
		d := compose.New(me(a), in.To)
		d.Cc, d.Bcc, d.Subject, d.Attach = in.Cc, in.Bcc, in.Subject, in.Attach
		if in.Body != "" {
			d.Body = compose.Signed(in.Body, me(a))
		}
		given := in.Body != "" || in.Subject != "" || len(in.To)+len(in.Cc)+len(in.Bcc)+len(in.Attach) > 0
		return createDraft(ctx, env, DraftInfo{Account: a.Name, Kind: KindNew}, d, given)
	},
}

// DraftReply answers a message.
var DraftReply = Op[DraftReplyIn, DraftOut]{
	Name: "draft.reply", Summary: "Start a reply to a message", Perm: Draft,
	Run: func(ctx context.Context, env *Env, in DraftReplyIn) (DraftOut, error) {
		if err := headerValues(slices.Concat(in.Cc, in.Attach)...); err != nil {
			return DraftOut{}, err
		}
		if in.Resume && (in.Body != "" || len(in.Cc)+len(in.Attach) > 0) {
			return DraftOut{}, usageError("resuming a draft takes no body, copies or files: edit the draft")
		}
		m, _, o, err := source(ctx, env, in.Account, in.ID)
		if err != nil {
			return DraftOut{}, err
		}
		if in.Resume {
			if out, ok, err := resumeReply(ctx, env, m.Account, m.Hash); err != nil || ok {
				return out, err
			}
		}
		a, err := account(env, m.Account)
		if err != nil {
			return DraftOut{}, err
		}
		kind := KindReply
		if in.All {
			kind = KindReplyAll
		}
		d := compose.Reply(o, me(a), in.All)
		d.Body, d.Cc, d.Attach = above(in.Body, d.Body), append(d.Cc, in.Cc...), in.Attach
		return createDraft(ctx, env, DraftInfo{Account: a.Name, Kind: kind, SourceHash: m.Hash}, d,
			in.Body != "" || len(in.Cc)+len(in.Attach) > 0)
	},
}

// DraftForward forwards a message.
var DraftForward = Op[DraftForwardIn, DraftOut]{
	Name: "draft.forward", Summary: "Start forwarding a message", Perm: Draft,
	Run: func(ctx context.Context, env *Env, in DraftForwardIn) (DraftOut, error) {
		if err := headerValues(slices.Concat(in.To, in.Cc, in.Attach)...); err != nil {
			return DraftOut{}, err
		}
		m, _, o, err := source(ctx, env, in.Account, in.ID)
		if err != nil {
			return DraftOut{}, err
		}
		a, err := account(env, m.Account)
		if err != nil {
			return DraftOut{}, err
		}
		d := compose.Forward(o, me(a))
		d.To, d.Cc, d.Attach, d.Body = in.To, in.Cc, in.Attach, above(in.Body, d.Body)
		return createDraft(ctx, env, DraftInfo{Account: a.Name, Kind: KindForward, SourceHash: m.Hash}, d,
			in.Body != "" || len(in.To)+len(in.Cc)+len(in.Attach) > 0)
	},
}

// headerValues refuses line breaks and other control characters in values
// a caller gives for header lines: in the editor file, "a\nBcc: x" would
// become a header of its own.
func headerValues(values ...string) error {
	for _, v := range values {
		if strings.ContainsFunc(v, unicode.IsControl) {
			return usageError("%q contains a line break or another control character", v)
		}
	}
	return nil
}

// above puts text written in advance (by a script) where the cursor would
// start: above the quote or the forwarded message.
func above(text, body string) string {
	if text == "" {
		return body
	}
	return strings.TrimRight(text, "\n") + "\n\n" + strings.TrimLeft(body, "\n")
}

// DraftUpdate reads a draft's editor file again, after editing. A new
// draft whose file the editor left as it was is dropped (Discarded), the
// way quitting the editor without saving abandons a message.
var DraftUpdate = Op[DraftIDIn, DraftOut]{
	Name: "draft.update", Summary: "Read a draft again after editing it", Perm: Draft,
	Run: func(ctx context.Context, env *Env, in DraftIDIn) (DraftOut, error) {
		info, d, err := loadDraft(env, in.ID)
		if err != nil {
			return DraftOut{}, err
		}
		if info.Untouched != "" {
			text, err := os.ReadFile(info.Path)
			if err != nil {
				return DraftOut{}, err
			}
			if fileHash(string(text)) == info.Untouched {
				return DraftOut{DraftInfo: info, Draft: d, Problems: []compose.Problem{}, Discarded: true}, removeDraft(env, info.ID)
			}
		}
		info.Untouched = ""
		info.Subject, info.To, info.Updated = d.Subject, d.To, env.Now().UTC()
		if err := saveDraftInfo(env, info); err != nil {
			return DraftOut{}, err
		}
		return draftOut(ctx, env, info, d), nil
	},
}

// DraftSetIn changes a draft from a frontend's own inputs.
type DraftSetIn struct {
	ID     string   `json:"id"`
	To     string   `json:"to,omitempty"`     // the To line as typed; replaces the recipients, when given
	Attach []string `json:"attach,omitempty"` // files to add
}

// DraftSet changes a draft's recipients or adds attachments, rewriting its
// editor file.
var DraftSet = Op[DraftSetIn, DraftOut]{
	Name: "draft.set", Summary: "Change a draft's recipients or add attachments", Perm: Draft,
	Run: func(ctx context.Context, env *Env, in DraftSetIn) (DraftOut, error) {
		if err := headerValues(append([]string{in.To}, in.Attach...)...); err != nil {
			return DraftOut{}, err
		}
		info, d, err := loadDraft(env, in.ID)
		if err != nil {
			return DraftOut{}, err
		}
		if in.To != "" {
			d.To = compose.SplitAddresses(in.To)
		}
		d.Attach = append(d.Attach, in.Attach...)
		if err := writeDraftFile(info.Path, d); err != nil {
			return DraftOut{}, err
		}
		info.Untouched = "" // changed by the user, through the frontend
		info.Subject, info.To, info.Updated = d.Subject, d.To, env.Now().UTC()
		if err := saveDraftInfo(env, info); err != nil {
			return DraftOut{}, err
		}
		return draftOut(ctx, env, info, d), nil
	},
}

// DraftShow shows a draft and its problems.
var DraftShow = Op[DraftIDIn, DraftOut]{
	Name: "draft.show", Summary: "Show a draft", Perm: Read,
	Run: func(ctx context.Context, env *Env, in DraftIDIn) (DraftOut, error) {
		info, d, err := loadDraft(env, in.ID)
		if err != nil {
			return DraftOut{}, err
		}
		return draftOut(ctx, env, info, d), nil
	},
}

// DraftList lists drafts, the most recently changed first.
var DraftList = Op[struct{}, []DraftInfo]{
	Name: "draft.list", Summary: "List drafts", Perm: Read,
	Run: func(_ context.Context, env *Env, _ struct{}) ([]DraftInfo, error) {
		return listDrafts(env)
	},
}

// DraftDiscard deletes a draft.
var DraftDiscard = Op[DraftIDIn, struct{}]{
	Name: "draft.discard", Summary: "Delete a draft", Perm: Draft,
	Run: func(_ context.Context, env *Env, in DraftIDIn) (struct{}, error) {
		if !validID(in.ID) {
			return struct{}{}, usageError("%q is not a draft id", in.ID)
		}
		release, err := lockOutbox(env)
		if err != nil {
			return struct{}{}, err
		}
		defer release()
		// A queued message would still go out: take it out first.
		if e, ok, err := queued(env, in.ID); err != nil || ok {
			return struct{}{}, errors.Join(err, usageError("this draft is in the outbox: nou outbox cancel %s first", e.ID))
		}
		return struct{}{}, removeDraft(env, in.ID)
	},
}

// sizeLimit is the message size checked before sending; the server's own
// limit is enforced when sending.
const sizeLimit = 25 << 20

// draftOut is a draft as frontends get it; lists are [] rather than null in
// JSON.
func draftOut(ctx context.Context, env *Env, info DraftInfo, d compose.Draft) DraftOut {
	for _, l := range []*[]string{&d.To, &d.Cc, &d.Bcc, &d.Attach, &d.References} {
		if *l == nil {
			*l = []string{}
		}
	}
	problems := draftChecks(ctx, env, info, d)
	if problems == nil {
		problems = []compose.Problem{}
	}
	return DraftOut{DraftInfo: info, Draft: d, Problems: problems}
}

// draftChecks runs the checks with what the archive knows.
func draftChecks(ctx context.Context, env *Env, info DraftInfo, d compose.Draft) []compose.Problem {
	in := compose.CheckInput{Draft: d, ReplyAll: info.Kind == KindReplyAll, SizeLimit: sizeLimit,
		Attach: func(p string) (int64, error) {
			st, err := os.Stat(expandPath(p))
			switch {
			case err != nil:
				return 0, err
			case !st.Mode().IsRegular(): // a directory, or a pipe or device that would hang the send
				return 0, errors.New("is not a regular file")
			}
			return st.Size(), nil
		}}
	if a, err := account(env, info.Account); err == nil {
		in.Me = me(a)
	}
	if ix, err := env.Index(ctx); err == nil {
		if known, err := ix.Addresses(ctx, info.Account, 2000); err == nil {
			for _, k := range known {
				in.Known = append(in.Known, k.Address)
			}
		}
	}
	if info.Kind == KindReply || info.Kind == KindReplyAll {
		if _, _, o, err := source(ctx, env, info.Account, info.SourceHash); err == nil {
			in.Original = &o
		}
	}
	if info.Kind == KindForward {
		if _, raw, _, err := source(ctx, env, info.Account, info.SourceHash); err == nil {
			content, _ := mailmsg.Bodies(raw)
			in.CarriedAttachments = len(content.Attachments)
			for _, a := range content.Attachments {
				in.CarriedSize += a.Size
			}
		}
	}
	return compose.Check(in)
}

// expandPath expands a leading "~/".
func expandPath(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return p
}
