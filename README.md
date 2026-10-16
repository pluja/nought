# nought

**nought** (command: `nou`) is a privacy-first **Inbox Zero** mail client you can drive entirely
from the command line.

It archives your mail locally, in plain files you own, and keeps the server down to your inbox.
Everything it does is a command with `--json` output and stable exit codes, so shell scripts, cron
jobs and AI agents can use it as easily as you can. There is also a keyboard-driven TUI.

> **Status:** early development. Import, archive, search, tags, sync with verified server deletion,
> mirroring of what your phone does, and the TUI work. Sending, MCP, PGP and a GUI are planned (see
> `docs/superpowers/specs/`).

## Principles

- **Never lose mail.** A message is deleted from the server only after its local copy has been
  written, fsynced, read back and hash-verified, and only when you have enabled deletion for that
  account. Every deletion is journaled first.
- **Privacy by default.** No telemetry, no update checks, no remote content. HTML mail is never
  rendered: it is converted to text, and text hidden from view (preheaders, invisible blocks) is
  dropped. When a message has both versions, the converted HTML is shown; `v` in the TUI, or
  `nou msg show --prefer plain`, shows the sender's plain text, and `prefer_body = "plain"` in
  the config makes that the default.
- **No lock-in.** Mail is stored as one `.eml` file per message in a standard Maildir; tags are
  dumped in notmuch's format. neomutt, notmuch, grep, restic and rclone all work on it.
- **Simple and auditable.** Go, a single static binary, few dependencies.

## Build

```sh
go build -trimpath -o nou ./cmd/nou      # Go 1.26+, no cgo needed
```

## Quick start

```sh
# 1. Bring in existing mail, e.g. a Thunderbird Maildir/mbox export (safe: read-only on the source).
nou import ~/thunderbird-export --account personal --dry-run
nou import ~/thunderbird-export --account personal

# 2. Add your IMAP account: a wizard finds the server settings from your address
#    (offline first; online only if you agree), tests them and saves.
#    The password goes to the OS keyring (or use a password command).
nou account add
nou account test personal       # shows what sync will do with each server folder

# 3. Archive your mail. Deletion is off until you turn it on.
nou sync

# 4. Triage.
nou search tag:inbox
nou msg show a3f9c2e1b0d4
nou msg tag from:newsletter@example.org --add newsletter --remove inbox
nou msg archive tag:inbox date:..2026-09-30

# 5. When the archive looks right, let nou keep the server down to your inbox.
nou account set personal --deletion enabled
nou sync --dry-run && nou sync
```

**The server's inbox is your inbox.** Mail stays on the server until you deal with it. Once you
archive or delete it, here or on another device, it leaves the server at the next sync (with
deletion enabled; it is always archived first). Mail in other folders (Sent, Archive...) counts as
dealt with once it is archived. Drafts, templates, junk, spam and trash are never archived.

Each account's folder rules are in the config file; `nou account show <name>` lists them:

```toml
[[account.folder]]
match     = "INBOX"
keep_days = 0     # once dealt with, at least this many days on the server (default 0)
max_days  = 90    # optional: off the server after this, even if not dealt with

[[account.folder]]
match     = "Sent*"
keep_days = 7     # keep a week of sent mail visible on your phone
```

## Using a phone too

Keep any mail app on your phone (Thunderbird, FairEmail, ...) on the same account. It shows your
inbox: what is still to be dealt with. Both sides agree after each sync:

| You do this | on the phone, nought does | in nought, the phone sees |
|---|---|---|
| archive | takes it out of the inbox | it leaves the server's INBOX |
| delete | moves it to the nought trash (`trash_days`, at least 7 days) | it is gone |
| move it back to INBOX | puts it back in the inbox | – |

Read, flagged and tags stay local. Older mail lives in nought, not on the phone: save what you will
need on the go (a ticket, a boarding pass), or leave it in the inbox, or keep a folder on the
server with its own `keep_days` (say, `match = "Travel"` with `keep_days = 30`) and file it there.
Moving mail into an ignored folder (Junk, Drafts...) counts as deleting it. If many messages vanish from a folder at once, nought
assumes a server problem, changes nothing, and asks you to confirm with
`nou sync --accept-removals` (`:sync accept-removals` in the TUI).

The TUI (`nou tui`) syncs when it starts and then every 10 minutes, quietly unless something
changed. After a failure (say, you are offline) it waits twice as long each time, up to an hour.
Set the interval with `sync_every = "30m"` under `[tui]`, or turn it off with `"0"`; `S` syncs
at any time.

## Deleting mail

Some mail is not worth keeping anywhere: a login code, a shipping notice. Deleting it removes it
from your archive and, at the next sync, from the server too, even for accounts whose automatic
deletion is off. It never reaches your backups (see below).

```sh
nou msg delete subject:"verification code"   # to the trash: hidden, deleted for good after 30 days
nou msg restore tag:deleted                  # changed your mind
nou msg delete --now a3f9c2e1b0d4            # for good, at once (server copy at the next sync)
nou trash empty
```

In the TUI, `#` moves a message to the trash (`z` undoes it, `gt` shows the trash, with a
countdown on each message; `:empty-trash` empties it) and `D`
deletes it for good after asking.

Rules can tag new mail as it is archived, and make it expire:

```toml
trash_days = 30   # how long the trash keeps mail (0: until the next sync)

[[rule]]
query = "from:no-reply@bank.example subject:code"
expire_days = 1   # deleted for good, here and on the server, a day after it arrives

[[rule]]
query = "from:newsletter@example.org"
tags = ["newsletter"]
```

## Searching

Terms are ANDed; `a OR b` ORs two terms; `-term` negates (put `--` before a query that starts
with `-`).

| Term | Matches |
|---|---|
| `tag:todo` | tagged `todo` |
| `is:unread`, `is:flagged`, `is:inbox`, `is:replied` | state |
| `from:bob`, `to:alice`, `subject:invoice` | header text |
| `account:work` | one account |
| `date:2026-01-01..2026-03-31`, `date:2026-05-01..` | the Date header, in UTC |
| `id:<message-id>`, `hash:a3f9c2` | a specific message |
| `word`, `"exact phrase"` | full text |

## Your data

```
~/.config/nought/config.toml              accounts and folder rules (no passwords)
~/.local/share/nought/archive/<account>/  Maildir: one immutable file per message
~/.local/share/nought/tags.dump           all tags, notmuch dump format
~/.local/share/nought/expunge.journal     every server deletion, JSON lines
~/.local/share/nought/index.db            SQLite index (rebuild with 'nou reindex')
~/.local/share/nought/transient/          deleted and expiring mail, and its tags: never back up
```

`nou paths` prints the actual locations on your system (macOS and Windows differ).

**Backups:** back up `archive/`, `tags.dump`, `expunge.journal` and `config.toml`, and never
`transient/`: it holds the mail you asked to be rid of. Message files
never change content and are named by their SHA-256, so restic and kopia deduplicate them
perfectly; with rclone, use `--track-renames`. Skip `index.db`: `nou reindex` rebuilds it from the
files and the tag dumps. A typical cron job:

```sh
nou sync && restic backup ~/.local/share/nought ~/.config/nought \
  --exclude index.db --exclude transient
```

## For scripts and agents

- Every command accepts `--json`. Errors go to stderr; with `--json` they are
  `{"error":{"code":"...","message":"..."}}`.
- `nou` never prompts when stdin is not a terminal. State-changing commands support `--dry-run`.
- Exit codes:

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | unexpected error (`internal`) |
| 2 | bad arguments or query (`usage`) |
| 3 | some items failed, others succeeded (`partial`) |
| 4 | permission denied (`permission`) |
| 5 | not found, or an ambiguous id (`not_found`, `ambiguous`) |
| 6 | another sync or reindex is running (`busy`) |
| 130 | interrupted |

## Development

```sh
make check     # lint (vet, staticcheck, golangci-lint), tests with coverage floor, govulncheck, build
make test      # go test -race ./...
```

## License

AGPL-3.0-or-later. See `LICENSE`.
