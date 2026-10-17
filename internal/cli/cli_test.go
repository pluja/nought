// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"

	"github.com/pluja/nought/internal/imaptest"
	"github.com/pluja/nought/internal/ops"
	"github.com/pluja/nought/internal/secret"
	"github.com/pluja/nought/internal/smtptest"
)

var update = flag.Bool("update", false, "update testscript golden output")

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"nou": func() {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			code := Run(ctx, Options{
				Args: os.Args[1:], Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
				Getenv: os.Getenv, Version: "test", Secrets: fileSecrets(os.Getenv("NOU_TEST_KEYRING")),
				TLS: testTLS(os.Getenv("NOU_TEST_CA")), Interactive: testInteractive(),
			})
			stop()
			os.Exit(code)
		},
	})
}

func TestScripts(t *testing.T) {
	testscript.Run(t, scriptParams(t, filepath.Join("testdata", "script")))
}

// TestExamples runs the scripts in examples/ against test data, so a change
// to nou's JSON output cannot break them unnoticed. They need a POSIX sh
// and jq.
func TestExamples(t *testing.T) {
	examples, err := filepath.Abs(filepath.Join("..", "..", "examples"))
	if err != nil {
		t.Fatal(err)
	}
	p := scriptParams(t, filepath.Join("testdata", "examples"))
	setup := p.Setup
	p.Setup = func(e *testscript.Env) error {
		e.Setenv("EXAMPLES", examples)
		return setup(e)
	}
	testscript.Run(t, p)
}

// scriptParams runs the scripts in dir with one IMAP and one SMTP test
// server for all of them.
func scriptParams(t *testing.T, dir string) testscript.Params {
	t.Helper()
	srv := imaptest.Start(t, imaptest.Options{})
	smtpSrv := smtptest.Start(t, smtptest.Options{})
	return testscript.Params{
		Dir:           dir,
		UpdateScripts: *update,
		Cmds: map[string]func(*testscript.TestScript, bool, []string){
			"code":       cmdCode,
			"imapappend": func(ts *testscript.TestScript, neg bool, args []string) { cmdIMAPAppend(t, srv, ts, neg, args) },
			"imapcount":  func(ts *testscript.TestScript, neg bool, args []string) { cmdIMAPCount(t, srv, ts, neg, args) },
			"expandenv":  cmdExpandEnv,
			"capture":    cmdCapture,
			"smtpcount":  func(ts *testscript.TestScript, neg bool, args []string) { cmdSMTPCount(smtpSrv, ts, neg, args) },
			"smtpfail":   func(ts *testscript.TestScript, neg bool, args []string) { cmdSMTPFail(smtpSrv, ts, neg, args) },
		},
		Setup: func(e *testscript.Env) error {
			e.Setenv("NOUGHT_HOME", filepath.Join(e.WorkDir, "home"))
			e.Setenv("NOU_TEST_KEYRING", filepath.Join(e.WorkDir, "keyring.json"))
			ca := filepath.Join(e.WorkDir, "test-ca.pem")
			e.Setenv("NOU_TEST_CA", ca)
			e.Setenv("IMAP_PORT", strconv.Itoa(srv.Port))
			e.Setenv("SMTP_PORT", strconv.Itoa(smtpSrv.Port))
			return os.WriteFile(ca, append(slices.Clone(srv.CAPEM), smtpSrv.CAPEM...), 0o600)
		},
	}
}

// cmdCapture sets a variable from the last command's output, the first
// group of a regexp matched line by line:
//
//	capture ID '^draft ([0-9a-f]{32})'
func cmdCapture(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 2 {
		ts.Fatalf("usage: capture <name> <regexp>")
	}
	m := regexp.MustCompile("(?m)" + args[1]).FindStringSubmatch(ts.ReadFile("stdout"))
	if len(m) < 2 {
		ts.Fatalf("no match for %s in stdout", args[1])
	}
	ts.Setenv(args[0], m[1])
}

// cmdSMTPCount checks how many messages the SMTP server received:
//
//	smtpcount 2
func cmdSMTPCount(srv *smtptest.Server, ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: smtpcount <n>")
	}
	if got := strconv.Itoa(len(srv.Messages())); got != args[0] {
		ts.Fatalf("the SMTP server received %s messages, want %s", got, args[0])
	}
}

// cmdSMTPFail makes the SMTP server answer DATA with a code (0: accept):
//
//	smtpfail 451
func cmdSMTPFail(srv *smtptest.Server, ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: smtpfail <code>")
	}
	code, err := strconv.Atoi(args[0])
	if err != nil {
		ts.Fatalf("bad code %q", args[0])
	}
	srv.SetFailData(code)
}

// cmdExpandEnv replaces $VARS in a script file with their values:
//
//	expandenv answers.txt
func cmdExpandEnv(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) != 1 {
		ts.Fatalf("usage: expandenv <file>")
	}
	expanded := os.Expand(ts.ReadFile(args[0]), ts.Getenv)
	ts.Check(os.WriteFile(ts.MkAbs(args[0]), []byte(expanded), 0o600))
}

// testInteractive lets scripts drive the wizard through stdin.
func testInteractive() *bool {
	if os.Getenv("NOU_TEST_INTERACTIVE") != "1" {
		return nil
	}
	yes := true
	return &yes
}

// testTLS trusts the test server's certificate in nou subprocesses.
func testTLS(caFile string) *tls.Config {
	if caFile == "" {
		return nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

// cmdIMAPAppend appends a script file to a mailbox:
//
//	imapappend INBOX 30 mail.eml     (received 30 days ago)
func cmdIMAPAppend(t *testing.T, srv *imaptest.Server, ts *testscript.TestScript, neg bool, args []string) {
	t.Helper()
	if neg || len(args) != 3 {
		ts.Fatalf("usage: imapappend <mailbox> <days-ago> <file>")
	}
	days, err := strconv.Atoi(args[1])
	if err != nil {
		ts.Fatalf("bad days %q", args[1])
	}
	srv.Append(t, args[0], ts.ReadFile(args[2]), time.Now().Add(-time.Duration(days)*24*time.Hour))
}

// cmdIMAPCount checks how many messages a mailbox holds:
//
//	imapcount INBOX 2
func cmdIMAPCount(t *testing.T, srv *imaptest.Server, ts *testscript.TestScript, neg bool, args []string) {
	t.Helper()
	if neg || len(args) != 2 {
		ts.Fatalf("usage: imapcount <mailbox> <n>")
	}
	if got := strconv.Itoa(len(srv.UIDs(t, args[0]))); got != args[1] {
		ts.Fatalf("%s has %s messages, want %s", args[0], got, args[1])
	}
}

// cmdCode runs a command and checks its exit status:
//
//	code 2 nou bogus
func cmdCode(ts *testscript.TestScript, neg bool, args []string) {
	if neg || len(args) < 2 {
		ts.Fatalf("usage: code <status> <command> [args...]")
	}
	want, err := strconv.Atoi(args[0])
	if err != nil {
		ts.Fatalf("bad status %q", args[0])
	}
	got := 0
	if err := ts.Exec(args[1], args[2:]...); err != nil {
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			ts.Fatalf("run %s: %v", args[1], err)
		}
		got = exitErr.ExitCode()
	}
	if got != want {
		ts.Fatalf("exit status %d, want %d", got, want)
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		err      error
		wantCode int
		wantName string
	}{
		{fmt.Errorf("x: %w", ops.ErrUsage), exitUsage, "usage"},
		{fmt.Errorf("x: %w", ops.ErrPartial), exitPartial, "partial"},
		{fmt.Errorf("x: %w", ops.ErrPermission), exitPermission, "permission"},
		{fmt.Errorf("x: %w", ops.ErrAmbiguous), exitNotFound, "ambiguous"},
		{fmt.Errorf("x: %w", ops.ErrNotFound), exitNotFound, "not_found"},
		{fmt.Errorf("x: %w", ops.ErrBusy), exitBusy, "busy"},
		{fmt.Errorf("x: %w", context.Canceled), exitInterrupted, "interrupted"},
		{errors.New("boom"), exitInternal, "internal"},
	}
	for _, tt := range tests {
		code, name := exitCode(tt.err)
		if code != tt.wantCode || name != tt.wantName {
			t.Errorf("exitCode(%v) = %d, %q; want %d, %q", tt.err, code, name, tt.wantCode, tt.wantName)
		}
	}
}

func TestBuildInfoDefaultsToDev(t *testing.T) {
	if got := buildInfo("").Version; got == "" {
		t.Error("empty version")
	}
	if got := buildInfo("v1.2.3").Version; got != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", got)
	}
}

// fileSecrets is a test keyring persisted in a JSON file, so state survives
// across the separate nou processes of a test script.
type fileSecrets string

func (f fileSecrets) load() map[string]string {
	m := map[string]string{}
	if data, err := os.ReadFile(string(f)); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	return m
}

func (f fileSecrets) save(m map[string]string) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(string(f), data, 0o600)
}

func (f fileSecrets) Get(account string) (string, error) {
	pw, ok := f.load()[account]
	if !ok {
		return "", secret.ErrNotFound
	}
	return pw, nil
}

func (f fileSecrets) Set(account, password string) error {
	m := f.load()
	m[account] = password
	return f.save(m)
}

func (f fileSecrets) Delete(account string) error {
	m := f.load()
	if _, ok := m[account]; !ok {
		return secret.ErrNotFound
	}
	delete(m, account)
	return f.save(m)
}

func TestPrintSyncOtherDevices(t *testing.T) {
	var b strings.Builder
	out := ops.SyncOut{Accounts: []ops.AccountSyncOut{{Account: "p", Deletion: true, ArchivedElsewhere: 1, DeletedElsewhere: 2,
		BackInInbox: 1, TrashCleaned: 3, Notes: []string{"held"}}}}
	if err := printSync(&b, out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"on another device: 1 archived, 2 deleted, 1 moved back to the inbox",
		"emptied 3 from the server's trash", "note: held"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, b.String())
		}
	}
}

// Review: the shell turns subject:"verification code" into one argument,
// subject:verification code; joined with spaces, it became two terms and
// matched (and could delete for good) other mail. An argument keeps the
// meaning the shell gave it.
func TestQueryFromArgs(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"tag:inbox", "is:unread"}, "tag:inbox is:unread"},
		{[]string{"subject:verification code"}, `subject:"verification code"`},
		{[]string{"-from:Big Shop", "tag:inbox"}, `-from:"Big Shop" tag:inbox`},
		{[]string{"two words"}, `"two words"`},
		{[]string{`subject:"already quoted"`}, `subject:"already quoted"`},
		{[]string{"a3f9c2e1b0d4"}, "a3f9c2e1b0d4"},
		{[]string{"date:2026-01-01..2026-02-01"}, "date:2026-01-01..2026-02-01"},
	}
	for _, tt := range tests {
		if got := queryFromArgs(tt.args); got != tt.want {
			t.Errorf("queryFromArgs(%q) = %q, want %q", tt.args, got, tt.want)
		}
	}
}
