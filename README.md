# nought

**nought** (command: `nou`) is a privacy-first, AI-first **Inbox Zero** mail client.

It archives your mail locally, in plain files you own, and keeps the server empty. Every
feature is a command with `--json` output and stable exit codes, so scripts and AI agents can
use it as easily as you can.

> **Status:** early development. The core works: import, archive, search, tag, sync, and
> verified server deletion. Sending, AI triage, MCP, PGP, a TUI and a GUI are planned (see
> `docs/superpowers/specs/`).

## Principles

- **Never lose mail.** A message is deleted from the server only after its local copy has been
  written, fsynced, read back and hash-verified, and only when you have enabled deletion for that
  account. Every deletion is journaled first.
- **Privacy by default.** No telemetry, no update checks, no remote content. HTML mail is never
  rendered: it is converted to text.
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

# 2. Add your IMAP account. The password goes to the OS keyring (or use --password-command).
nou account add personal --email me@example.org --imap-host imap.example.org
nou account test personal       # shows what sync will do with each server folder

# 3. Archive your mail. Deletion is off until you turn it on.
nou sync

# 4. Triage.
nou search tag:inbox
nou msg show a3f9c2e1b0d4
nou msg tag from:newsletter@example.org --add newsletter --remove inbox
nou msg archive tag:inbox date:..2026-09-30

# 5. When the archive looks right, let nou keep the server empty.
nou account set personal --deletion enabled
nou sync --dry-run && nou sync
```

By default, mail stays on the server for 7 days after it arrives (so your phone still sees recent
mail). Drafts, templates, junk, spam and trash are never archived or deleted. Each account's
folder rules are in the config file, and `nou account show <name>` lists them.

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
```

`nou paths` prints the actual locations on your system (macOS and Windows differ).

**Backups:** back up `archive/`, `tags.dump`, `expunge.journal` and `config.toml`. Message files
never change content and are named by their SHA-256, so restic and kopia deduplicate them
perfectly; with rclone, use `--track-renames`. Skip `index.db`: `nou reindex` rebuilds it from the
files and the tag dump. A typical cron job:

```sh
nou sync && restic backup ~/.local/share/nought ~/.config/nought --exclude index.db
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
