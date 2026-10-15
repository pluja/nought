// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"nought/internal/index"
	"nought/internal/query"
	"nought/internal/store"
)

// TagDeleted marks mail in the trash.
const TagDeleted = "deleted"

// dateLayout formats delete-after dates.
const dateLayout = "2006-01-02"

func deleteAfterTag(t time.Time) string {
	return index.DeleteAfterPrefix + t.UTC().Format(dateLayout)
}

// isTransient reports whether a message belongs in the transient store:
// it was deleted, or it expires.
func isTransient(tags []string) bool {
	return slices.ContainsFunc(tags, func(t string) bool {
		return t == TagDeleted || IsExpiryTag(t)
	})
}

// IsExpiryTag reports whether a tag is a delete-after date.
func IsExpiryTag(t string) bool { return strings.HasPrefix(t, index.DeleteAfterPrefix) }

// ExpiresAt returns when a message is due to be deleted for good: the
// earliest of its delete-after dates.
func ExpiresAt(tags []string) (time.Time, bool) {
	var first time.Time
	for _, t := range tags {
		d, ok := strings.CutPrefix(t, index.DeleteAfterPrefix)
		if !ok {
			continue
		}
		if at, err := time.Parse(dateLayout, d); err == nil && (first.IsZero() || at.Before(first)) {
			first = at
		}
	}
	return first, !first.IsZero()
}

// leavesServerAt returns when sync removes a message's server copies
// because the user deleted it or it expired, whatever the account's
// deletion setting.
func leavesServerAt(tags []string, now time.Time) (time.Time, bool) {
	if slices.Contains(tags, TagDeleted) {
		return now, true
	}
	at, ok := ExpiresAt(tags)
	if ok && at.Before(now) {
		at = now
	}
	return at, ok
}

// TrashTags returns the tag change that moves a message to the trash, or
// (now) deletes it for good at the next chance.
func TrashTags(env *Env, now bool) (add, remove []string, err error) {
	cfg, err := env.Config()
	if err != nil {
		return nil, nil, err
	}
	when := env.Now().AddDate(0, 0, cfg.TrashKeepDays())
	if now {
		when = env.Now()
	}
	return []string{TagDeleted, deleteAfterTag(when)}, []string{TagInbox}, nil
}

// placeFiles moves message files between the archive and the transient
// store so that deleted and expiring mail stays out of backups.
func placeFiles(env *Env, msgs []index.Message) error {
	var errs []error
	for _, m := range msgs {
		archive, err := env.Store(m.Account)
		if err != nil {
			return err
		}
		transient, err := env.Transient(m.Account)
		if err != nil {
			return err
		}
		from, to := archive, transient
		if !isTransient(m.Tags) {
			from, to = transient, archive
		}
		if from.Has(m.Hash) {
			if err := from.MoveTo(to, m.Hash); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", m.Hash[:IDLen], err))
			}
		}
	}
	return errors.Join(errs...)
}

// applyRules runs the configured rules on a newly archived message: it
// adds their tags and, for expiring rules, a delete-after date.
func applyRules(ctx context.Context, env *Env, ix *index.Index, m *index.Message) error {
	cfg, err := env.Config()
	if err != nil || len(cfg.Rules) == 0 {
		return err
	}
	var add []string
	for _, r := range cfg.Rules {
		// The rule's terms are ANDed with the message's address.
		q, err := query.Parse(r.Query + " hash:" + m.Hash + " account:" + m.Account)
		if err != nil {
			continue
		}
		n, err := ix.Count(ctx, q)
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		add = append(add, r.Tags...)
		if r.ExpireDays > 0 {
			add = append(add, deleteAfterTag(env.Now().AddDate(0, 0, r.ExpireDays)))
		}
	}
	if len(add) == 0 {
		return nil
	}
	if err := ix.ModifyTags(ctx, []int64{m.ID}, add, nil); err != nil {
		return err
	}
	m.Tags = mergeTags(m.Tags, add)
	return placeFiles(env, []index.Message{*m})
}

// purge deletes for good the messages whose delete-after date has come,
// unless a copy is still on a server: that copy is removed by the next
// sync first (it needs the local file to verify it is the same message).
func purge(ctx context.Context, env *Env) (int, error) {
	ix, err := env.Index(ctx)
	if err != nil {
		return 0, err
	}
	due, err := ix.PurgeDue(ctx, env.Now().UTC().Format(dateLayout))
	if err != nil {
		return 0, err
	}
	purged := 0
	var errs []error
	for _, m := range due {
		on, err := ix.OnServer(ctx, m.Account, []string{m.Hash})
		if err != nil {
			return purged, err
		}
		if len(on[m.Hash]) > 0 {
			continue
		}
		st, err := env.locate(m.Account, m.Hash)
		if err != nil {
			return purged, err
		}
		if err := st.Remove(m.Hash); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		if err := ix.DeleteMessage(ctx, m); err != nil {
			errs = append(errs, err)
			continue
		}
		purged++
	}
	if purged > 0 {
		errs = append(errs, writeTagsDump(ctx, env))
	}
	return purged, errors.Join(errs...)
}

// DeleteIn selects messages to delete.
type DeleteIn struct {
	Query   string `json:"query"`
	Account string `json:"account,omitempty"` // narrows a bare message id
	// Now deletes for good at once instead of moving to the trash.
	Now    bool `json:"now,omitempty"`
	DryRun bool `json:"dry_run,omitempty"`
}

// DeleteOut reports a deletion.
type DeleteOut struct {
	Matched int  `json:"matched"`
	Purged  int  `json:"purged"`  // deleted for good now
	Pending int  `json:"pending"` // waiting for the next sync to remove server copies
	DryRun  bool `json:"dry_run"`
}

// MsgDelete moves messages to the trash (kept out of backups, removed for
// good after trash_days), or deletes them for good at once. Copies still
// on a server are deleted there at the next sync.
var MsgDelete = Op[DeleteIn, DeleteOut]{
	Name:    "msg.delete",
	Summary: "Delete messages: to the trash, or for good with now",
	Perm:    Delete,
	Run: func(ctx context.Context, env *Env, in DeleteIn) (DeleteOut, error) {
		add, remove, err := TrashTags(env, in.Now)
		if err != nil {
			return DeleteOut{}, err
		}
		tag, err := runTag(ctx, env, TagIn{Query: in.Query, Account: in.Account, DryRun: in.DryRun,
			Add: add, Remove: remove})
		out := DeleteOut{Matched: tag.Matched, DryRun: in.DryRun}
		if err != nil || in.DryRun || !in.Now {
			return out, err
		}
		out.Purged, err = purge(ctx, env)
		out.Pending = max(out.Matched-out.Purged, 0)
		return out, err
	},
}

// RestoreIn selects messages to take out of the trash.
type RestoreIn struct {
	Query   string `json:"query"`
	Account string `json:"account,omitempty"`
}

// MsgRestore takes messages out of the trash (and cancels their expiry),
// back into the archive and the inbox.
var MsgRestore = Op[RestoreIn, TagOut]{
	Name:    "msg.restore",
	Summary: "Restore deleted or expiring messages to the inbox",
	Perm:    Tag,
	Run: func(ctx context.Context, env *Env, in RestoreIn) (TagOut, error) {
		q, err := queryOrID(ctx, env, in.Query, in.Account)
		if err != nil {
			return TagOut{}, err
		}
		ix, err := env.Index(ctx)
		if err != nil {
			return TagOut{}, err
		}
		var out TagOut
		var changed []index.Message
		err = ix.Each(ctx, q, func(page []index.Message) error {
			for _, m := range page {
				out.Matched++
				var remove []string
				for _, t := range m.Tags {
					if t == TagDeleted || strings.HasPrefix(t, index.DeleteAfterPrefix) {
						remove = append(remove, t)
					}
				}
				if len(remove) == 0 {
					continue
				}
				if err := ix.ModifyTags(ctx, []int64{m.ID}, []string{TagInbox}, remove); err != nil {
					return err
				}
				m.Tags = applyTagChange(m.Tags, []string{TagInbox}, remove)
				changed = append(changed, m)
				out.Changed++
			}
			return nil
		})
		if err != nil {
			return out, err
		}
		if err := placeFiles(env, changed); err != nil {
			return out, err
		}
		if out.Changed > 0 {
			return out, writeTagsDump(ctx, env)
		}
		return out, nil
	},
}

// TrashEmptyOut reports emptying the trash.
type TrashEmptyOut struct {
	Purged  int `json:"purged"`
	Pending int `json:"pending"` // waiting for the next sync to remove server copies
}

// TrashEmpty deletes everything in the trash for good.
var TrashEmpty = Op[struct{}, TrashEmptyOut]{
	Name:    "trash.empty",
	Summary: "Delete everything in the trash for good",
	Perm:    Delete,
	Run: func(ctx context.Context, env *Env, _ struct{}) (TrashEmptyOut, error) {
		ix, err := env.Index(ctx)
		if err != nil {
			return TrashEmptyOut{}, err
		}
		q, _ := query.Parse("tag:" + TagDeleted)
		today := deleteAfterTag(env.Now())
		total := 0
		err = ix.Each(ctx, q, func(page []index.Message) error {
			for _, m := range page {
				total++
				var old []string
				for _, t := range m.Tags {
					if strings.HasPrefix(t, index.DeleteAfterPrefix) && t != today {
						old = append(old, t)
					}
				}
				if err := ix.ModifyTags(ctx, []int64{m.ID}, []string{today}, old); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return TrashEmptyOut{}, err
		}
		purged, err := purge(ctx, env)
		return TrashEmptyOut{Purged: purged, Pending: max(total-purged, 0)}, err
	},
}
