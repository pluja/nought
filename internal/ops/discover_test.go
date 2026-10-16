// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/pluja/nought/internal/autoconfig"
	"github.com/pluja/nought/internal/imaptest"
)

func TestAccountDiscover(t *testing.T) {
	env := newTestEnv(t)
	used := false
	env.Network = autoconfig.Network{
		LookupSRV: func(_ context.Context, service, _, name string) (string, []*net.SRV, error) {
			used = true
			if service == "imaps" && name == "custom.example" {
				return "", []*net.SRV{{Target: "imap.custom.example.", Port: 993}}, nil
			}
			return "", nil, errors.New("none")
		},
		LookupMX: func(context.Context, string) ([]*net.MX, error) { return nil, errors.New("none") },
		ProbeTLS: func(context.Context, string, int) bool { return false },
	}

	res, err := Call(t.Context(), env, Read, AccountDiscover, DiscoverIn{Email: "me@posteo.de"})
	if err != nil || res.IMAP.Host != "posteo.de" || used {
		t.Fatalf("built-in = %+v, %v (network used: %v)", res, err, used)
	}
	if _, err := Call(t.Context(), env, Read, AccountDiscover, DiscoverIn{Email: "me@custom.example"}); !errors.Is(err, ErrNotFound) || used {
		t.Fatalf("offline unknown = %v (network used: %v)", err, used)
	}
	if _, err := Call(t.Context(), env, Read, AccountDiscoverOnline, DiscoverIn{Email: "me@custom.example"}); !errors.Is(err, ErrPermission) || used {
		t.Fatalf("online discovery at read level = %v (network used: %v)", err, used)
	}
	res, err = Call(t.Context(), env, Delete, AccountDiscoverOnline, DiscoverIn{Email: "me@custom.example"})
	if err != nil || res.Source != autoconfig.SourceSRV || res.IMAP.Host != "imap.custom.example" {
		t.Fatalf("online = %+v, %v", res, err)
	}
	if _, err := Call(t.Context(), env, Read, AccountDiscover, DiscoverIn{Email: "nope"}); !errors.Is(err, ErrUsage) {
		t.Errorf("bad email = %v", err)
	}
}

func TestAccountCheck(t *testing.T) {
	env := newTestEnv(t)
	srv := imaptest.Start(t, imaptest.Options{})
	env.TLS = srv.Client
	cfg := srv.Config()
	in := AccountIn{Email: imaptest.User, IMAPHost: cfg.Host, IMAPPort: cfg.Port, Password: imaptest.Password}
	if _, err := Call(t.Context(), env, Draft, AccountCheck, in); !errors.Is(err, ErrPermission) {
		t.Fatalf("check below delete level = %v", err)
	}
	out, err := Call(t.Context(), env, Delete, AccountCheck, in)
	if err != nil || !out.UIDPlus || len(out.Folders) != 3 {
		t.Fatalf("check = %+v, %v", out, err)
	}
	in.Password = ""
	in.PasswordCommand = "echo " + imaptest.Password
	if _, err := Call(t.Context(), env, Delete, AccountCheck, in); err != nil {
		t.Errorf("check with command = %v", err)
	}
	in.PasswordCommand = ""
	in.Password = "wrong"
	if _, err := Call(t.Context(), env, Delete, AccountCheck, in); err == nil {
		t.Error("wrong password accepted")
	}
	if _, err := Call(t.Context(), env, Delete, AccountCheck, AccountIn{Email: "x"}); !errors.Is(err, ErrUsage) {
		t.Errorf("invalid settings = %v", err)
	}
	// Nothing was saved.
	if list, _ := Call(t.Context(), env, Read, AccountList, struct{}{}); len(list) != 0 {
		t.Errorf("check saved an account: %+v", list)
	}
}
