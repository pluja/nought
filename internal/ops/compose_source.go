// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"fmt"

	"github.com/pluja/nought/internal/compose"
	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/index"
	"github.com/pluja/nought/internal/mailmsg"
	"github.com/pluja/nought/internal/render"
)

// sendingAccount is the named account, or the only one when no name is
// given.
func sendingAccount(env *Env, name string) (*config.Account, error) {
	if name != "" {
		return account(env, name)
	}
	cfg, err := env.Config()
	if err != nil {
		return nil, err
	}
	if len(cfg.Accounts) != 1 {
		return nil, usageError("several accounts: say which one sends")
	}
	return &cfg.Accounts[0], nil
}

func me(a *config.Account) compose.Me {
	return compose.Me{Addresses: a.Addresses(), Signature: a.Signature}
}

// source loads an archived message for a reply or forward: its index entry,
// its raw bytes, and what compose needs from it.
func source(ctx context.Context, env *Env, account, id string) (index.Message, []byte, compose.Original, error) {
	m, err := resolve(ctx, env, account, id)
	if err != nil {
		return m, nil, compose.Original{}, err
	}
	st, err := env.locate(m.Account, m.Hash)
	if err != nil {
		return m, nil, compose.Original{}, err
	}
	raw, err := st.Get(m.Hash)
	if err != nil {
		return m, nil, compose.Original{}, fmt.Errorf("read message %s: %w", m.Hash[:IDLen], err)
	}
	h, _ := mailmsg.Parse(raw) // best effort: mail is hostile and often malformed
	content, _ := mailmsg.Bodies(raw)
	body := content.Plain
	if body == "" && content.HTML != "" {
		body = render.HTMLToText(content.HTML)
	}
	// Quoted into the editor file and sent on to others: as clean as the
	// reader shows it (no CR, which editors show as ^M, nor invisible or
	// bidirectional controls from a hostile sender).
	o := compose.Original{From: h.From, ReplyTo: h.ReplyTo, To: h.To, Cc: h.Cc, Subject: h.Subject,
		MessageID: h.MessageID, References: h.References, Body: render.Sanitize(body)}
	if !h.Date.IsZero() {
		o.Date = h.Date.Local().Format("Mon, 02 Jan 2006 15:04")
	}
	return m, raw, o, nil
}
