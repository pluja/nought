// SPDX-License-Identifier: AGPL-3.0-or-later

package ops

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"nought/internal/config"
	"nought/internal/imapsync"
	"nought/internal/imaptest"
)

// hasNote reports whether any account or folder note contains substr.
func hasNote(out SyncOut, substr string) bool {
	for _, a := range out.Accounts {
		for _, n := range a.Notes {
			if strings.Contains(n, substr) {
				return true
			}
		}
		for _, f := range a.Folders {
			for _, n := range f.Notes {
				if strings.Contains(n, substr) {
					return true
				}
			}
		}
	}
	return false
}

func setTrashDays(t *testing.T, env *Env, days int) {
	t.Helper()
	cfg, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	next := *cfg
	next.TrashDays = &days
	if err := saveConfig(env, &next); err != nil {
		t.Fatal(err)
	}
}

func TestPhoneArchiveLeavesTheInbox(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	srv.CreateMailbox(t, "Archive")
	uid := srv.Append(t, "INBOX", mail("p@x", "archived on the phone"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.Move(t, "INBOX", uid, "Archive")

	out := runSyncOK(t, env, SyncIn{})
	if out.Accounts[0].ArchivedElsewhere != 1 || len(search(t, env, "tag:inbox")) != 0 {
		t.Fatalf("not mirrored: %+v", out.Accounts[0])
	}
	runSyncOK(t, env, SyncIn{})
	if n := len(srv.UIDs(t, "Archive")); n != 0 {
		t.Errorf("handled mail still in the server's Archive: %d", n)
	}
	if got := search(t, env, `subject:"archived on the phone"`); len(got) != 1 {
		t.Error("the local copy must stay")
	}
}

func TestPhoneDeleteGoesToTheTrash(t *testing.T) {
	for _, how := range []string{"expunge", "move to Trash"} {
		t.Run(how, func(t *testing.T) {
			env, srv := syncEnv(t, imaptest.Options{}, true)
			setTrashDays(t, env, 0)
			uid := srv.Append(t, "INBOX", mail("d@x", "deleted on the phone"), daysAgo(env, 0))
			runSyncOK(t, env, SyncIn{})
			if how == "expunge" {
				srv.Expunge(t, "INBOX", uid)
			} else {
				srv.Move(t, "INBOX", uid, "Trash")
			}
			out := runSyncOK(t, env, SyncIn{})
			if out.Accounts[0].DeletedElsewhere != 1 || len(search(t, env, "tag:deleted")) != 1 {
				t.Fatalf("not trashed: %+v", out.Accounts[0])
			}
			// At least 7 days in the nought trash, even with trash_days = 0.
			later := daysAgo(env, -6)
			env.Now = func() time.Time { return later }
			runSyncOK(t, env, SyncIn{})
			if len(search(t, env, "tag:deleted")) != 1 {
				t.Error("purged before the 7-day safety margin")
			}
		})
	}
}

func TestMovedBackToTheInbox(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	srv.CreateMailbox(t, "Archive")
	uid := srv.Append(t, "INBOX", mail("b@x", "back again"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.Move(t, "INBOX", uid, "Archive")
	runSyncOK(t, env, SyncIn{})
	if len(search(t, env, "tag:inbox")) != 0 {
		t.Fatal("phone archive not mirrored")
	}
	srv.Move(t, "Archive", srv.UIDs(t, "Archive")[0], "INBOX")
	out := runSyncOK(t, env, SyncIn{})
	if out.Accounts[0].BackInInbox != 1 || len(search(t, env, "tag:inbox")) != 1 {
		t.Errorf("not back in the inbox: %+v", out.Accounts[0])
	}
}

func TestMovedBackRestoresFromTheTrash(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	uid := srv.Append(t, "INBOX", mail("r@x", "rescued"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.Move(t, "INBOX", uid, "Trash")
	runSyncOK(t, env, SyncIn{})
	if len(search(t, env, "tag:deleted")) != 1 {
		t.Fatal("phone delete not mirrored")
	}
	srv.Move(t, "Trash", srv.UIDs(t, "Trash")[0], "INBOX")
	runSyncOK(t, env, SyncIn{})
	if got := search(t, env, "tag:inbox -tag:deleted"); len(got) != 1 {
		t.Errorf("not restored: %v", got)
	}
}

// Review Focus 1 and 2: a copy (not a move) and a duplicate delivery leave
// a live INBOX copy, so the message stays in the inbox.
func TestCopyOrDuplicateKeepsTheInbox(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	srv.CreateMailbox(t, "Archive")
	raw := mail("dup@x", "twice")
	first := srv.Append(t, "INBOX", raw, daysAgo(env, 0))
	srv.Append(t, "INBOX", raw, daysAgo(env, 0))
	copied := srv.Append(t, "INBOX", mail("c@x", "copied"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.Expunge(t, "INBOX", first)
	srv.Copy(t, "INBOX", copied, "Archive")
	runSyncOK(t, env, SyncIn{})
	if got := search(t, env, "tag:inbox"); len(got) != 2 {
		t.Fatalf("inbox = %v; both still have a live INBOX copy", got)
	}
	// Archived in nought: every server copy goes.
	archiveMail(t, env, "subject:copied")
	runSyncOK(t, env, SyncIn{})
	if n := len(srv.UIDs(t, "Archive")); n != 0 {
		t.Errorf("the Archive copy of handled mail stayed: %d", n)
	}
}

func TestNoClassificationWithoutACleanRun(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	uid := srv.Append(t, "INBOX", mail("x@x", "kept"), daysAgo(env, 0))
	srv.CreateMailbox(t, "Other")
	srv.Append(t, "Other", mail("y@x", "y"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.Expunge(t, "INBOX", uid)
	srv.Recreate(t, "Other") // UIDVALIDITY changes: this run is not clean
	out := runSyncOK(t, env, SyncIn{})
	if len(search(t, env, "tag:inbox")) != 1 || out.Accounts[0].DeletedElsewhere != 0 || !hasNote(out, "applied next time") {
		t.Fatalf("classified after an incomplete run: %+v", out.Accounts[0])
	}
	out = runSyncOK(t, env, SyncIn{}) // clean now
	if out.Accounts[0].DeletedElsewhere != 1 {
		t.Errorf("not applied on the next clean run: %+v", out.Accounts[0])
	}
}

func TestMassRemovalNeedsConfirmation(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	var uids []uint32
	for i := range 30 {
		uids = append(uids, srv.Append(t, "INBOX", mail(fmt.Sprintf("m%d@x", i), fmt.Sprintf("m%d", i)), daysAgo(env, 0)))
	}
	runSyncOK(t, env, SyncIn{})
	for _, u := range uids[:25] {
		srv.Expunge(t, "INBOX", u)
	}
	out := runSyncOK(t, env, SyncIn{})
	if len(search(t, env, "tag:inbox")) != 30 || !hasNote(out, "accept-removals") || !out.Accounts[0].RemovalsHeld ||
		hasNote(out, "did not sync completely") {
		t.Fatalf("mass removal applied without confirmation: %+v", out.Accounts[0])
	}
	out = runSyncOK(t, env, SyncIn{AcceptRemovals: true})
	if len(search(t, env, "tag:deleted")) != 25 || out.Accounts[0].DeletedElsewhere != 25 {
		t.Errorf("accepted removals not applied: %+v", out.Accounts[0])
	}
}

// Review Focus 3 and 4.
func TestDeletedFolderAndNoUIDPlus(t *testing.T) {
	t.Run("folder deleted on the server", func(t *testing.T) {
		env, srv := syncEnv(t, imaptest.Options{}, false)
		srv.CreateMailbox(t, "Receipts")
		for i := range 25 {
			srv.Append(t, "Receipts", mail(fmt.Sprintf("r%d@x", i), "receipt"), daysAgo(env, 0))
		}
		srv.Append(t, "INBOX", mail("i@x", "inbox mail"), daysAgo(env, 0))
		runSyncOK(t, env, SyncIn{})
		srv.DeleteMailbox(t, "Receipts")
		out := runSyncOK(t, env, SyncIn{})
		if !hasNote(out, "accept-removals") || len(search(t, env, "tag:inbox")) != 1 {
			t.Fatalf("whole-folder removal not held: %+v", out.Accounts[0])
		}
		runSyncOK(t, env, SyncIn{AcceptRemovals: true})
		ix, err := env.Index(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		st, _ := ix.ServerStatus(t.Context(), "personal")
		if st.OnServer != 1 || len(search(t, env, "tag:inbox")) != 1 || len(search(t, env, "subject:receipt")) != 25 {
			t.Errorf("after accepting: %d on server, inbox %v", st.OnServer, search(t, env, "tag:inbox"))
		}
	})
	t.Run("no UIDPLUS", func(t *testing.T) {
		env, srv := syncEnv(t, imaptest.Options{NoUIDPlus: true}, true)
		srv.CreateMailbox(t, "Archive")
		uid := srv.Append(t, "INBOX", mail("p@x", "phone archive"), daysAgo(env, 0))
		runSyncOK(t, env, SyncIn{})
		srv.Move(t, "INBOX", uid, "Archive")
		out := runSyncOK(t, env, SyncIn{})
		if out.Accounts[0].ArchivedElsewhere != 1 || len(srv.UIDs(t, "Archive")) != 1 {
			t.Errorf("mirroring must work and deletion must not: %+v", out.Accounts[0])
		}
	})
}

func TestMirrorDryRunChangesNothing(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	uid := srv.Append(t, "INBOX", mail("d@x", "deleted on the phone"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.Expunge(t, "INBOX", uid)
	out := runSyncOK(t, env, SyncIn{DryRun: true})
	if out.Accounts[0].DeletedElsewhere != 1 || len(search(t, env, "tag:inbox")) != 1 {
		t.Fatalf("dry run: %+v, inbox %v", out.Accounts[0], search(t, env, "tag:inbox"))
	}
	if out := runSyncOK(t, env, SyncIn{}); out.Accounts[0].DeletedElsewhere != 1 {
		t.Errorf("the real run after a dry run: %+v", out.Accounts[0])
	}
}

func TestServerTrashIsEmptiedOfKnownMail(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	known := srv.Append(t, "INBOX", mail("k@x", "known"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.Move(t, "INBOX", known, "Trash")
	srv.Append(t, "Trash", mail("u@x", "never archived"), daysAgo(env, 0))
	// Review Focus 5: the same Message-ID with different content stays.
	srv.Append(t, "Trash", strings.Replace(mail("k@x", "known"), "Subject: known", "Subject: resent", 1), daysAgo(env, 0))

	out := runSyncOK(t, env, SyncIn{})
	if out.Accounts[0].TrashCleaned != 1 || out.Accounts[0].DeletedElsewhere != 1 {
		t.Fatalf("cleaned %d, deleted elsewhere %d; want 1 and 1", out.Accounts[0].TrashCleaned, out.Accounts[0].DeletedElsewhere)
	}
	if n := len(srv.UIDs(t, "Trash")); n != 2 {
		t.Errorf("server trash has %d, want the unknown and the resent one", n)
	}
	entries := readJournal(t, env)
	if last := entries[len(entries)-1]; last.Folder != "Trash" || last.Hash != "" || last.MessageID != "" {
		t.Errorf("trash cleanup must be journaled without identifying the message: %+v", last)
	}
	// Examined messages are not looked at again.
	if out := runSyncOK(t, env, SyncIn{}); out.Accounts[0].TrashCleaned != 0 {
		t.Errorf("second run cleaned %d", out.Accounts[0].TrashCleaned)
	}
}

func TestServerTrashUntouchedWithDeletionOff(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	uid := srv.Append(t, "INBOX", mail("k@x", "known"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.Move(t, "INBOX", uid, "Trash")
	runSyncOK(t, env, SyncIn{})
	if len(srv.UIDs(t, "Trash")) != 1 {
		t.Error("touched the server's trash with deletion off")
	}
}

func TestServerTrashFolder(t *testing.T) {
	a := &config.Account{Folders: append(DefaultFolderRules(), config.FolderRule{Match: "Papierkorb", Action: config.ActionIgnore})}
	for _, c := range []struct {
		folders []imapsync.Folder
		want    string
	}{
		{[]imapsync.Folder{{Name: "INBOX"}, {Name: "Papierkorb", Raw: "P", Trash: true}, {Name: "Trash"}}, "Papierkorb"},
		{[]imapsync.Folder{{Name: "INBOX"}, {Name: "INBOX/Deleted Items"}}, "INBOX/Deleted Items"},
		{[]imapsync.Folder{{Name: "INBOX"}, {Name: "Archive"}}, ""},
		// A trash the rules archive is handled like any folder.
		{[]imapsync.Folder{{Name: "INBOX"}, {Name: "Bin", Trash: true}}, ""},
	} {
		f, ok := serverTrash(a, c.folders)
		if f.Name != c.want || ok != (c.want != "") {
			t.Errorf("serverTrash(%v) = %q, %v; want %q", c.folders, f.Name, ok, c.want)
		}
	}
}

// A phone archive made while a sync runs (between two folders' scans) is
// never read as a deletion.
func TestPhoneArchiveDuringASync(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	srv.CreateMailbox(t, "Archive")
	uid := srv.Append(t, "INBOX", mail("race@x", "archived mid-sync"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	moved := false
	during := SyncIn{Progress: func(_ string, f FolderSyncOut) {
		if f.Folder == "Archive" && !moved { // after Archive was scanned
			moved = true
			srv.Move(t, "INBOX", uid, "Archive")
		}
	}}
	runSyncOK(t, env, during)
	runSyncOK(t, env, SyncIn{})
	if got := search(t, env, "tag:deleted"); len(got) != 0 {
		t.Fatalf("a phone archive was read as a deletion: %v", got)
	}
	if len(srv.UIDs(t, "Archive")) != 1 || len(search(t, env, "tag:inbox")) != 0 {
		t.Errorf("not mirrored as an archive: server Archive %v, inbox %v", srv.UIDs(t, "Archive"), search(t, env, "tag:inbox"))
	}
}

// Backstop: a message taken for deleted elsewhere that turns up under a new
// UID in a kept folder comes out of the trash.
func TestDeletedMessageFoundInAKeptFolder(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, false)
	srv.CreateMailbox(t, "Archive")
	uid := srv.Append(t, "INBOX", mail("lost@x", "found again"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.Expunge(t, "INBOX", uid)
	runSyncOK(t, env, SyncIn{})
	if len(search(t, env, "tag:deleted")) != 1 {
		t.Fatal("setup: not trashed")
	}
	srv.Append(t, "Archive", mail("lost@x", "found again"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	if got := search(t, env, "tag:deleted OR tag:inbox"); len(got) != 0 {
		t.Errorf("found in Archive, it must be archived (not deleted, not in the inbox): %v", got)
	}
}

// The trash cleanup keeps its own place: it never makes the folder's own
// sync skip mail, should the trash be archived later.
func TestTrashCleanupKeepsItsOwnPlace(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	srv.Append(t, "Trash", mail("u@x", "unknown in the trash"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	setFolderRules(t, env, nil) // no rules: the trash is archived like any folder
	runSyncOK(t, env, SyncIn{})
	if got := search(t, env, `subject:"unknown in the trash"`); len(got) != 1 {
		t.Errorf("trash mail skipped after the trash became archived: %v", got)
	}
}

// Review finding: a folder list without INBOX must never read as "the
// whole inbox was deleted".
func TestMissingInboxChangesNothing(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	srv.Append(t, "INBOX", mail("a@x", "still here"), daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.DeleteMailbox(t, "INBOX")
	out, err := Call(t.Context(), env, Delete, SyncRun, SyncIn{AcceptRemovals: true})
	if err == nil || out.Accounts[0].Error == "" || !strings.Contains(out.Accounts[0].Error, "INBOX") {
		t.Errorf("sync without INBOX = %+v, %v", out.Accounts[0], err)
	}
	if len(search(t, env, "tag:inbox")) != 1 || len(search(t, env, "tag:deleted")) != 0 {
		t.Error("a missing INBOX changed the inbox")
	}
}

func TestBothDuplicateCopiesDeletedOnThePhone(t *testing.T) {
	env, srv := syncEnv(t, imaptest.Options{}, true)
	raw := mail("dup@x", "twice")
	a := srv.Append(t, "INBOX", raw, daysAgo(env, 0))
	b := srv.Append(t, "INBOX", raw, daysAgo(env, 0))
	runSyncOK(t, env, SyncIn{})
	srv.Expunge(t, "INBOX", a)
	srv.Expunge(t, "INBOX", b)
	if out := runSyncOK(t, env, SyncIn{}); out.Accounts[0].DeletedElsewhere != 1 {
		t.Errorf("deleted elsewhere = %d, want 1", out.Accounts[0].DeletedElsewhere)
	}
}
