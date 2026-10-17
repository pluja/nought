// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pluja/nought/internal/config"
	"github.com/pluja/nought/internal/imaptest"
	"github.com/pluja/nought/internal/lock"
	"github.com/pluja/nought/internal/smtptest"
)

// sendEnv is a test env whose account "personal" (me@example.org) sends
// through a test SMTP server, with the sample mail imported.
func sendEnv(t *testing.T, opts smtptest.Options) (*Env, *smtptest.Server) {
	t.Helper()
	env := newTestEnv(t)
	srv := smtptest.Start(t, opts)
	env.TLS, env.SMTPTimeout = trust(t, srv.CAPEM), 5*time.Second
	cfg := srv.Config()
	addAccount(t, env, AccountIn{Name: "personal", Email: "me@example.org", Username: smtptest.User,
		IMAPHost: "127.0.0.1", IMAPPort: 1, SMTPHost: cfg.Host, SMTPPort: cfg.Port, SMTPTLS: cfg.TLS,
		Password: smtptest.Password})
	importSample(t, env)
	return env, srv
}

// trust builds a TLS config that trusts the given test certificates.
func trust(t *testing.T, pems ...[]byte) *tls.Config {
	t.Helper()
	pool := x509.NewCertPool()
	for _, p := range pems {
		if !pool.AppendCertsFromPEM(p) {
			t.Fatal("bad test certificate")
		}
	}
	return &tls.Config{RootCAs: pool}
}

func newDraft(t *testing.T, env *Env, to, subject, body string) DraftOut {
	t.Helper()
	d, err := Call(t.Context(), env, Draft, DraftNew, DraftNewIn{To: []string{to}, Subject: subject, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// flushNow flushes as if every undo window had passed.
func flushNow(t *testing.T, env *Env) FlushOut {
	t.Helper()
	later := env.Now().Add(time.Hour)
	env.Now = func() time.Time { return later }
	out, err := Call(t.Context(), env, Send, OutboxFlush, FlushIn{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func markState(t *testing.T, env *Env, id, state string) {
	t.Helper()
	e, err := loadEntry(env, id)
	if err != nil {
		t.Fatal(err)
	}
	e.State = state
	b, _ := json.Marshal(e)
	if err := os.WriteFile(entryPath(env, id, ".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// setDraftHeader replaces a header line in a draft's editor file.
func setDraftHeader(t *testing.T, env *Env, d DraftOut, name, value string) {
	t.Helper()
	text, err := os.ReadFile(d.Path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(text), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, name+":") {
			lines[i] = name + ": " + value
		}
	}
	if err := os.WriteFile(d.Path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Draft, DraftUpdate, DraftIDIn{ID: d.ID}); err != nil {
		t.Fatal(err)
	}
}

func TestSendUndoFlushArchive(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	d := newDraft(t, env, "alice@example.org", "Hello", "Hi Alice\n")
	entry, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID})
	if err != nil || entry.State != StateWaiting || !entry.SendAfter.After(env.Now()) {
		t.Fatalf("send = %+v, %v", entry, err)
	}
	// Undo within the window: back to the draft, nothing sent.
	back, err := Call(t.Context(), env, Draft, OutboxCancel, DraftIDIn{ID: entry.ID})
	if err != nil || back.ID != d.ID || len(srv.Messages()) != 0 {
		t.Fatalf("undo = %+v, %v", back, err)
	}
	entry, err = Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID})
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := Call(t.Context(), env, Send, OutboxFlush, FlushIn{}); out.Sent != 0 || out.Waiting != 1 {
		t.Errorf("flush inside the window = %+v", out)
	}
	if out := flushNow(t, env); out.Sent != 1 || len(srv.Messages()) != 1 {
		t.Fatalf("flush = %+v", out)
	}
	if got := search(t, env, "tag:folder:sent"); !slices.Equal(got, []string{"Hello"}) {
		t.Errorf("sent copy = %v", got)
	}
	if got := search(t, env, "tag:inbox OR tag:unread"); slices.Contains(got, "Hello") {
		t.Error("the sent copy is in the inbox or unread")
	}
	if _, err := Call(t.Context(), env, Draft, OutboxCancel, DraftIDIn{ID: entry.ID}); err == nil {
		t.Error("undo after sending succeeded")
	}
	if list, _ := Call(t.Context(), env, Read, DraftList, struct{}{}); len(list) != 0 {
		t.Errorf("the draft stayed after sending: %+v", list)
	}
	if list, _ := Call(t.Context(), env, Read, OutboxList, struct{}{}); len(list) != 0 {
		t.Errorf("the outbox entry stayed after sending: %+v", list)
	}
}

func TestReplySentMarksTheOriginalReplied(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	d, err := Call(t.Context(), env, Draft, DraftReply, DraftReplyIn{ID: idOf(t, env, `subject:"read one"`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID, Now: true}); err != nil {
		t.Fatal(err)
	}
	flushNow(t, env)
	if got := search(t, env, "is:replied"); !slices.Equal(got, []string{"read one"}) {
		t.Errorf("replied = %v", got)
	}
	if m := srv.Messages(); len(m) != 1 || !bytes.Contains(m[0].Data, []byte("In-Reply-To: <1@x>")) {
		t.Errorf("sent = %s", m)
	}
}

func TestBccIsSentButNotShown(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	d := newDraft(t, env, "alice@example.org", "B", "x\n")
	setDraftHeader(t, env, d, "Bcc", "secret@example.org")
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID, Now: true}); err != nil {
		t.Fatal(err)
	}
	flushNow(t, env)
	got := srv.Messages()
	if len(got) != 1 || !slices.Contains(got[0].To, "secret@example.org") || bytes.Contains(got[0].Data, []byte("secret@")) {
		t.Fatalf("bcc: %+v", got)
	}
	ix, _ := env.Index(t.Context())
	m, _ := ix.Resolve(t.Context(), "personal", idOf(t, env, "subject:B tag:folder:sent"))
	st, _ := env.Store("personal")
	stored, _ := st.Get(m.Hash)
	if !bytes.Contains(stored, []byte("secret@example.org")) {
		t.Error("the local copy lost the Bcc recipients")
	}
}

func TestBlockingProblemsRefuseToSend(t *testing.T) {
	env, _ := sendEnv(t, smtptest.Options{})
	d := newDraft(t, env, "alice@example.org", "x", "x\n")
	setDraftHeader(t, env, d, "To", "")
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID}); err == nil || !strings.Contains(err.Error(), "no recipients") {
		t.Errorf("send without recipients: %v", err)
	}
}

func TestTemporaryRetryPermanentHold(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{FailData: 451})
	d := newDraft(t, env, "alice@example.org", "T", "x\n")
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID, Now: true}); err != nil {
		t.Fatal(err)
	}
	if out := flushNow(t, env); out.Failed != 0 || out.Waiting != 1 {
		t.Errorf("temporary failure = %+v (kept waiting for a retry)", out)
	}
	srv.SetFailData(554)
	if out := flushNow(t, env); out.Failed != 1 {
		t.Errorf("permanent failure = %+v", out)
	}
	list, _ := Call(t.Context(), env, Read, OutboxList, struct{}{})
	if len(list) != 1 || list[0].State != StateFailed || list[0].LastError == "" {
		t.Fatalf("outbox = %+v", list)
	}
	srv.SetFailData(0)
	if _, err := Call(t.Context(), env, Send, OutboxRetry, DraftIDIn{ID: list[0].ID}); err != nil {
		t.Fatal(err)
	}
	if out := flushNow(t, env); out.Sent != 1 {
		t.Errorf("retry = %+v", out)
	}
}

func TestSendingStateIsNeverResent(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	d := newDraft(t, env, "alice@example.org", "Crash", "x\n")
	e, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID, Now: true})
	if err != nil {
		t.Fatal(err)
	}
	markState(t, env, e.ID, StateSending) // as if nought died during the SMTP transaction
	if out := flushNow(t, env); out.Sent != 0 || len(srv.Messages()) != 0 || len(out.Notes) == 0 {
		t.Errorf("a 'sending' entry was retried: %+v", out)
	}
}

func TestQuitFlushOfflineKeepsTheMessage(t *testing.T) { // Review Focus 5
	env, srv := sendEnv(t, smtptest.Options{})
	d := newDraft(t, env, "alice@example.org", "Q", "x\n")
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID}); err != nil {
		t.Fatal(err)
	}
	srv.Close() // offline
	out, err := Call(t.Context(), env, Send, OutboxFlush, FlushIn{All: true})
	list, _ := Call(t.Context(), env, Read, OutboxList, struct{}{})
	if err != nil || out.Sent != 0 || len(list) != 1 || list[0].State != StateWaiting {
		t.Errorf("offline quit flush: %+v, %v, outbox %+v", out, err, list)
	}
}

func TestForwardCarriesAttachments(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	root := t.TempDir() + "/export"
	writeFile(t, root+"/Inbox/cur/"+maildirName("9", "S"), "From: a@example.org\r\nMessage-ID: <att@x>\r\nSubject: with file\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nsee file\r\n"+
		"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=doc.pdf\r\n\r\n%PDF\r\n--b--\r\n")
	if _, err := Call(t.Context(), env, Delete, ImportRun, ImportIn{Path: root, Account: "personal"}); err != nil {
		t.Fatal(err)
	}
	f, err := Call(t.Context(), env, Draft, DraftForward, DraftForwardIn{ID: idOf(t, env, `subject:"with file"`)})
	if err != nil {
		t.Fatal(err)
	}
	setDraftHeader(t, env, f, "To", "bob@example.org")
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: f.ID, Now: true}); err != nil {
		t.Fatal(err)
	}
	flushNow(t, env)
	if m := srv.Messages(); len(m) != 1 || !bytes.Contains(m[0].Data, []byte("doc.pdf")) {
		t.Errorf("forward lost its attachment")
	}
}

// attachIMAP gives the account a test IMAP server, with save_sent set.
func attachIMAP(t *testing.T, env *Env, srv *smtptest.Server, saveSent string) *imaptest.Server {
	t.Helper()
	imapSrv := imaptest.Start(t, imaptest.Options{})
	env.TLS = trust(t, srv.CAPEM, imapSrv.CAPEM)
	cfg, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	next := *cfg
	next.Accounts = slices.Clone(cfg.Accounts)
	next.Accounts[0].IMAP, next.Accounts[0].SaveSent, next.Accounts[0].Username = imapSrv.Config(), saveSent, imaptest.User
	next.Accounts[0].Folders = DefaultFolderRules()
	if err := saveConfig(env, &next); err != nil {
		t.Fatal(err)
	}
	return imapSrv
}

func TestSyncFlushesTheOutbox(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	attachIMAP(t, env, srv, config.SaveSentLocal)
	d := newDraft(t, env, "alice@example.org", "Due", "x\n")
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID, Now: true}); err != nil {
		t.Fatal(err)
	}
	if out := runSyncOK(t, env, SyncIn{DryRun: true}); out.Outbox.Sent != 0 || len(srv.Messages()) != 0 {
		t.Fatalf("a dry run sent mail: %+v", out.Outbox)
	}
	if out := runSyncOK(t, env, SyncIn{}); out.Outbox.Sent != 1 || len(srv.Messages()) != 1 {
		t.Errorf("sync did not send the due message: %+v", out.Outbox)
	}
}

func TestSaveSentOnServer(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	imapSrv := attachIMAP(t, env, srv, config.SaveSentServer)
	d := newDraft(t, env, "alice@example.org", "Copied", "x\n")
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID, Now: true}); err != nil {
		t.Fatal(err)
	}
	out := flushNow(t, env)
	if n := len(imapSrv.UIDs(t, "Sent")); n != 1 || len(srv.Messages()) != 1 {
		t.Fatalf("server Sent has %d (%+v)", n, out)
	}
	sync := runSyncOK(t, env, SyncIn{})
	if f := folder(t, sync, "Sent"); f.Fetched != 1 || len(search(t, env, "subject:copied")) != 1 {
		t.Errorf("the appended copy was not recognised as the same message: %+v", f)
	}
}

// Review: a message whose fate is unknown is never sent again by itself.
func TestLostReplyIsNeverResent(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{DropData: true})
	d := newDraft(t, env, "alice@example.org", "Maybe", "x\n")
	e, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID, Now: true})
	if err != nil {
		t.Fatal(err)
	}
	flushNow(t, env)
	out := flushNow(t, env)
	got, err := loadEntry(env, e.ID)
	if err != nil || got.State != StateSending || len(srv.Messages()) != 1 || len(out.Notes) == 0 {
		t.Errorf("entry %+v, %v; %d received; notes %q", got, err, len(srv.Messages()), out.Notes)
	}
}

// Review: another nou process (a cron sync, the TUI) may be sending.
func TestOutboxLockedByAnotherProcess(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	d := newDraft(t, env, "alice@example.org", "Once", "x\n")
	e, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID, Now: true})
	if err != nil {
		t.Fatal(err)
	}
	held, err := lock.Acquire(outboxLock(env))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Send, OutboxFlush, FlushIn{All: true}); !errors.Is(err, ErrBusy) || len(srv.Messages()) != 0 {
		t.Errorf("flush while locked: %v", err)
	}
	if _, err := Call(t.Context(), env, Draft, OutboxCancel, DraftIDIn{ID: e.ID}); !errors.Is(err, ErrBusy) {
		t.Errorf("cancel while locked: %v", err)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if out := flushNow(t, env); out.Sent != 1 {
		t.Errorf("after release: %+v", out)
	}
}

// Review: sending a draft twice must not queue it twice; a queued draft
// cannot be discarded (it would still go out).
func TestQueuedDraftIsQueuedOnce(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	d := newDraft(t, env, "alice@example.org", "Twice", "x\n")
	e, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID, Now: true}); !errors.Is(err, ErrUsage) {
		t.Errorf("second send: %v", err)
	}
	if _, err := Call(t.Context(), env, Draft, DraftDiscard, DraftIDIn{ID: d.ID}); !errors.Is(err, ErrUsage) {
		t.Errorf("discard while queued: %v", err)
	}
	if _, err := loadEntry(env, e.ID); err != nil {
		t.Fatal(err)
	}
	if out := flushNow(t, env); out.Sent != 1 || len(srv.Messages()) != 1 {
		t.Errorf("flush: %+v, %d received", out, len(srv.Messages()))
	}
}

// Review: the flush says what became of each message it tried, so
// frontends need not guess from the outbox listing.
func TestFlushReportsEachEntry(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	ok := newDraft(t, env, "alice@example.org", "Fine", "x\n")
	sent, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: ok.ID, Now: true})
	if err != nil {
		t.Fatal(err)
	}
	out := flushNow(t, env)
	if len(out.Results) != 1 || out.Results[0].ID != sent.ID || out.Results[0].State != StateSent {
		t.Fatalf("results = %+v", out.Results)
	}
	srv.SetFailData(550)
	bad := newDraft(t, env, "alice@example.org", "Refused", "x\n")
	refused, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: bad.ID, Now: true})
	if err != nil {
		t.Fatal(err)
	}
	out = flushNow(t, env)
	if len(out.Results) != 1 || out.Results[0].ID != refused.ID || out.Results[0].State != StateFailed || out.Results[0].LastError == "" {
		t.Errorf("results = %+v", out.Results)
	}
}

// Review: a message refused during a sync (cron, the TUI's auto sync) is
// not silent: the sync reports it and is partial.
func TestSyncReportsRefusedMail(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	attachIMAP(t, env, srv, config.SaveSentLocal)
	srv.SetFailData(550)
	d := newDraft(t, env, "alice@example.org", "Refused", "x\n")
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID, Now: true}); err != nil {
		t.Fatal(err)
	}
	out, err := Call(t.Context(), env, Delete, SyncRun, SyncIn{})
	if !errors.Is(err, ErrPartial) || out.Outbox.Failed != 1 {
		t.Errorf("sync: %v, outbox %+v", err, out.Outbox)
	}
}

// Deferred (a): mail queued for an account that was removed fails with a
// reason, instead of waiting forever.
func TestRemovedAccountFailsItsMail(t *testing.T) {
	env, _ := sendEnv(t, smtptest.Options{})
	d := newDraft(t, env, "alice@example.org", "Orphan", "x\n")
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := Call(t.Context(), env, Delete, AccountRemove, AccountName{Name: "personal"}); err != nil {
		t.Fatal(err)
	}
	out := flushNow(t, env)
	if len(out.Results) != 1 || out.Results[0].State != StateFailed || !strings.Contains(out.Results[0].LastError, "personal") {
		t.Errorf("results = %+v", out.Results)
	}
}

// Deferred (b): the bytes of an entry whose .json is gone (a crash while
// removing it) are cleaned up; they may hold Bcc recipients.
func TestFlushSweepsLeftoverFiles(t *testing.T) {
	env, _ := sendEnv(t, smtptest.Options{})
	id := strings.Repeat("ab", 16)
	writeFile(t, entryPath(env, id, ".eml"), "x")
	writeFile(t, entryPath(env, id, ".stored.eml"), "Bcc: secret@x.org\r\n")
	flushNow(t, env)
	for _, ext := range []string{".eml", ".stored.eml"} {
		if _, err := os.Stat(entryPath(env, id, ext)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left: %v", ext, err)
		}
	}
}

// Deferred (c): the copy for the server's Sent folder is recorded once
// made, so finishing an interrupted send does not append it again.
func TestSentCopyAppendedOnce(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	imapSrv := attachIMAP(t, env, srv, config.SaveSentServer)
	for _, appended := range []bool{true, false} {
		d := newDraft(t, env, "alice@example.org", "Once", "x\n")
		e, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID})
		if err != nil {
			t.Fatal(err)
		}
		e.State, e.Appended = StateSent, appended // SMTP done; the copy maybe too
		if err := saveEntry(env, e); err != nil {
			t.Fatal(err)
		}
		flushNow(t, env)
	}
	if n := len(imapSrv.UIDs(t, "Sent")); n != 1 {
		t.Errorf("Sent has %d copies, want 1 (only the one not yet appended)", n)
	}
}

// Deferred (g): a large message gets the time a slow uplink needs.
func TestSMTPTimeoutGrowsWithSize(t *testing.T) {
	if got := smtpTimeout(2*time.Minute, 1000); got != 2*time.Minute {
		t.Errorf("small: %v", got)
	}
	if got := smtpTimeout(2*time.Minute, 25<<20); got != 2*time.Minute+400*time.Second {
		t.Errorf("25 MB: %v", got)
	}
}

// Attached files are sent, typed by their extension ("~/" is the home
// directory, as in the editor's Attach: lines).
func TestAttachedFilesAreSent(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeFile(t, filepath.Join(home, "report.pdf"), "%PDF-1.4 report")
	odd := filepath.Join(t.TempDir(), "notes.zz-unknown")
	writeFile(t, odd, "raw notes")
	d, err := Call(t.Context(), env, Draft, DraftNew, DraftNewIn{To: []string{"alice@example.org"}, Subject: "Files",
		Body: "Both files attached.\n", Attach: []string{"~/report.pdf", odd}})
	if err != nil || len(d.Problems) != 0 {
		t.Fatalf("draft = %+v, %v", d.Problems, err)
	}
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID}); err != nil {
		t.Fatal(err)
	}
	if out := flushNow(t, env); out.Sent != 1 {
		t.Fatalf("flush = %+v", out)
	}
	sent := string(srv.Messages()[0].Data)
	for _, want := range []string{"application/pdf", "report.pdf", "application/octet-stream", "notes.zz-unknown"} {
		if !strings.Contains(sent, want) {
			t.Errorf("sent message lacks %q", want)
		}
	}
}

// A Sent copy the server would not take is reported, and the message is
// not sent again for it: it was delivered, and is archived locally.
func TestFailedSentCopyIsNotResent(t *testing.T) {
	env, srv := sendEnv(t, smtptest.Options{}) // its IMAP server does not answer
	if _, err := Call(t.Context(), env, Delete, AccountSet, AccountSetIn{Name: "personal", SaveSent: config.SaveSentServer}); err != nil {
		t.Fatal(err)
	}
	d := newDraft(t, env, "alice@example.org", "Copied", "x\n")
	if _, err := Call(t.Context(), env, Send, MsgSend, SendIn{DraftID: d.ID, Now: true}); err != nil {
		t.Fatal(err)
	}
	out := flushNow(t, env)
	if out.Sent != 1 || !slices.ContainsFunc(out.Notes, func(n string) bool { return strings.Contains(n, "not copied to the server's Sent folder") }) {
		t.Fatalf("flush = %+v", out)
	}
	if again := flushNow(t, env); again.Sent != 0 || len(srv.Messages()) != 1 {
		t.Errorf("sent again: %+v, %d delivered", again, len(srv.Messages()))
	}
	if got := search(t, env, "tag:folder:sent"); !slices.Equal(got, []string{"Copied"}) {
		t.Errorf("local sent copy = %v", got)
	}
}
