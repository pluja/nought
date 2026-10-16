// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/pluja/nought/internal/atomicfile"
	"github.com/pluja/nought/internal/compose"
	"github.com/pluja/nought/internal/index"
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
		m, _, o, err := source(ctx, env, in.Account, in.ID)
		if err != nil {
			return DraftOut{}, err
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
		entries, err := os.ReadDir(env.Paths.DraftsDir)
		if errors.Is(err, os.ErrNotExist) {
			return []DraftInfo{}, nil
		}
		if err != nil {
			return nil, err
		}
		out := []DraftInfo{}
		for _, e := range entries {
			id, ok := strings.CutSuffix(e.Name(), ".json")
			if !ok || !validID(id) {
				continue
			}
			info, err := loadDraftInfo(env, id)
			if err != nil {
				return nil, err
			}
			out = append(out, info)
		}
		slices.SortFunc(out, func(x, y DraftInfo) int { return y.Updated.Compare(x.Updated) })
		return out, nil
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

// SuggestIn asks for addresses starting with a prefix.
type SuggestIn struct {
	Account string `json:"account,omitempty"`
	Prefix  string `json:"prefix"`
}

// AddressSuggest completes addresses from the archive.
var AddressSuggest = Op[SuggestIn, []index.AddressCount]{
	Name: "address.suggest", Summary: "Complete an address from your archive", Perm: Read,
	Run: func(ctx context.Context, env *Env, in SuggestIn) ([]index.AddressCount, error) {
		a, err := sendingAccount(env, in.Account)
		if err != nil {
			return nil, err
		}
		ix, err := env.Index(ctx)
		if err != nil {
			return nil, err
		}
		all, err := ix.Addresses(ctx, a.Name, 2000)
		if err != nil {
			return nil, err
		}
		prefix := strings.ToLower(strings.TrimSpace(in.Prefix))
		out := []index.AddressCount{}
		for _, c := range all {
			if strings.HasPrefix(c.Address, prefix) || wordPrefix(strings.ToLower(c.Display), prefix) {
				out = append(out, c)
				if len(out) == 10 {
					break
				}
			}
		}
		return out, nil
	},
}

func wordPrefix(s, prefix string) bool {
	return slices.ContainsFunc(strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '"' || r == '<' }),
		func(w string) bool { return strings.HasPrefix(w, prefix) })
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

// createDraft saves a new draft. given: the caller supplied some of its
// content, so it is never dropped as untouched.
func createDraft(ctx context.Context, env *Env, info DraftInfo, d compose.Draft, given bool) (DraftOut, error) {
	id, err := newID()
	if err != nil {
		return DraftOut{}, err
	}
	now := env.Now().UTC()
	info.ID, info.Created, info.Updated = id, now, now
	info.Subject, info.To, info.InReplyTo, info.References = d.Subject, d.To, d.InReplyTo, d.References
	info.Path = draftPath(env, id, ".txt")
	if !given {
		info.Untouched = fileHash(compose.Render(d))
	}
	if err := writeDraftFile(info.Path, d); err != nil {
		return DraftOut{}, err
	}
	if err := saveDraftInfo(env, info); err != nil {
		return DraftOut{}, err
	}
	return draftOut(ctx, env, info, d), nil
}

func fileHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func writeDraftFile(path string, d compose.Draft) error {
	if err := atomicfile.Write(path, 0o600, func(w io.Writer) error {
		_, err := io.WriteString(w, compose.Render(d))
		return err
	}); err != nil {
		return fmt.Errorf("write draft: %w", err)
	}
	return nil
}

func saveDraftInfo(env *Env, info DraftInfo) error {
	return atomicfile.Write(draftPath(env, info.ID, ".json"), 0o600, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(info)
	})
}

func loadDraftInfo(env *Env, id string) (DraftInfo, error) {
	if !validID(id) {
		return DraftInfo{}, usageError("%q is not a draft id", id)
	}
	b, err := os.ReadFile(draftPath(env, id, ".json"))
	if errors.Is(err, os.ErrNotExist) {
		return DraftInfo{}, fmt.Errorf("draft %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return DraftInfo{}, err
	}
	var info DraftInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return DraftInfo{}, fmt.Errorf("draft %s: %w", id, err)
	}
	info.ID, info.Path = id, draftPath(env, id, ".txt") // never trust a path from disk
	return info, nil
}

// loadDraft reads a draft's details and its editor file.
func loadDraft(env *Env, id string) (DraftInfo, compose.Draft, error) {
	info, err := loadDraftInfo(env, id)
	if err != nil {
		return info, compose.Draft{}, err
	}
	text, err := os.ReadFile(info.Path)
	if err != nil {
		return info, compose.Draft{}, fmt.Errorf("draft %s: %w", id, err)
	}
	d, err := compose.Parse(string(text), compose.Draft{InReplyTo: info.InReplyTo, References: info.References})
	if err != nil {
		return info, d, usageError("draft %s: %v", id, err)
	}
	return info, d, nil
}

// removeDraft removes the metadata first: a crash in between leaves an
// unlisted file, never a draft that cannot be opened.
func removeDraft(env *Env, id string) error {
	var errs []error
	for _, ext := range []string{".json", ".txt"} {
		if err := os.Remove(draftPath(env, id, ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func draftPath(env *Env, id, ext string) string { return filepath.Join(env.Paths.DraftsDir, id+ext) }

// newID is a random id: nothing about the message is in file names.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// validID accepts only ids newID makes, so an id can never leave the
// drafts directory.
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}
