# Example scripts

Small POSIX `sh` scripts built on `nou`'s JSON output. They are starting points: read one before
you run it, and change it to taste. Each explains its settings at the top; this page is about
putting them to work.

| Script | Does | Needs |
|---|---|---|
| [`ai-classify.sh`](ai-classify.sh) | tags new mail by its content, with a language model | `jq`, `curl` |
| [`notify.sh`](notify.sh) | syncs, then shows a desktop notification with the count of new mail | `jq` |
| [`top-senders.sh`](top-senders.sh) | who fills your inbox, by message count | `jq` |
| [`save-invoices.sh`](save-invoices.sh) | saves the attachments of mail tagged `receipts` to a folder, once each | `jq` |
| [`backup.sh`](backup.sh) | backs up exactly what `nou paths` recommends | `jq`, `restic` or `rsync` |

They need a current `nou` (one that knows `NOU_PERM` and `*`; `ai-classify.sh` refuses an older one).

## Install

```sh
install -m 755 examples/*.sh ~/.local/bin/    # or run them from the checkout
```

## Run them after every sync

One small script does the round: sync, then tag and file what arrived. Put it in
`~/.local/bin/nou-sync`:

```sh
#!/bin/sh
# Sync, then tag and file the new mail. Each step runs even if one before it failed.
PATH=$HOME/.local/bin:$HOME/go/bin:$PATH
set -a
. "$HOME/.config/nought/scripts.env"
set +a
notify.sh          # the sync, and a notification for new mail
ai-classify.sh
save-invoices.sh
```

Settings go in `~/.config/nought/scripts.env`. The API key does not: keep it in a file only you
can read (`chmod 600`), or in your password manager.

```sh
NOU_AI_URL=https://api.example.com/v1
NOU_AI_REMOTE=1                     # only for an endpoint on another machine: your mail goes there
NOU_AI_MODEL=some-model
NOU_AI_KEY_COMMAND="cat $HOME/.config/nought/ai-key"
NOU_AI_TAGS="receipts newsletters notifications travel personal"
NOU_AI_EFFORT=low                   # sorting mail needs little thinking
```

On Linux, a systemd user timer runs it every ten minutes, with access to your desktop's
notifications (cron jobs have none). `~/.config/systemd/user/nou-sync.service`:

```ini
[Unit]
Description=Sync mail with nou, then tag and file it

[Service]
Type=oneshot
ExecStart=%h/.local/bin/nou-sync
```

`~/.config/systemd/user/nou-sync.timer`:

```ini
[Unit]
Description=Sync mail with nou every ten minutes

[Timer]
OnBootSec=2min
OnUnitActiveSec=10min

[Install]
WantedBy=timers.target
```

```sh
chmod +x ~/.local/bin/nou-sync
systemctl --user enable --now nou-sync.timer
journalctl --user -u nou-sync     # what the last runs did
```

Elsewhere, cron works too (`*/10 * * * * $HOME/.local/bin/nou-sync`), notifications aside. A
sync that finds another one running (the TUI syncs on its own) stops with exit code 6 and changes
nothing; the next run catches up.

## Tag mail with AI, step by step

1. **Pick the model.** A local one (Ollama, llama.cpp, vLLM) keeps your mail on this machine. Any
   OpenAI-compatible service works too, with `NOU_AI_REMOTE=1`: it then reads your mail.
2. **Queue new mail.** Add this rule to the config file (`nou paths` shows where it is). It tags
   each message as it arrives; mail already archived is never queued by it.

   ```toml
   [[rule]]
   query = "*"
   tags  = ["classify"]
   ```

3. **Try it on a slice.** Queue a week of mail, run the script, and look at the tags it chose:

   ```sh
   nou msg tag --add classify -- date:2026-10-01..
   ai-classify.sh
   nou tags list
   ```

4. **Settle the tags.** Either give it your list (`NOU_AI_TAGS`), or let it make them up:
   `NOU_AI_TAGS= NOU_AI_NEW_TAGS=1` starts from nothing, and each tag it makes up is offered for
   the messages after it. To merge two it made up:

   ```sh
   nou msg tag --add travel -- tag:trips
   nou msg tag --remove trips -- tag:trips
   ```

5. **Do the rest.** Queue everything not tagged yet, and run it once more. Quote the `*`, and put
   `--` before the query, or its `-tag:` terms read as options:

   ```sh
   nou msg tag --add classify --dry-run -- '*' -tag:receipts -tag:newsletters
   nou msg tag --add classify -- '*' -tag:receipts -tag:newsletters
   ai-classify.sh
   ```

Mail is hostile input: a message can try to talk the model into anything. The script runs `nou`
with `NOU_PERM=tag`, so it can never delete or send, and it applies only tags that pass its
checks: at worst, a message gets a wrong tag. Stop it at any time; messages it has not reached
keep `classify` for the next run.

## Backups

`backup.sh` backs up the archive, the tag dump, the config, drafts, the outbox and the expunge
journal; never the trash, nor the index (`nou reindex` rebuilds it). With restic, set its
repository and password as usual (`RESTIC_REPOSITORY`, `RESTIC_PASSWORD_COMMAND`), and run
`backup.sh restic` daily, from its own timer like the one above. `backup.sh rsync <dest>` makes a
plain copy instead.

## Write your own

- Every command takes `--json`, with stable fields; exit codes are in the main [README](../README.md).
- `NOU_PERM=read`, `tag`, `draft` or `send` caps what a script may do. Set it whenever a script
  acts on what mail says.
- Commands that change or delete mail or tags take `--dry-run`: try a script on your mail without
  changing it.
- Quote `'*'`, and put `--` before a query that starts with `-` (`-- -tag:done`).
- To process mail once: tag it on arrival with a rule, work through `tag:<it>`, and take the tag
  off each message as you go. A run that stops resumes where it left off.
- These scripts are tested against `nou` on every change (`internal/cli/testdata/examples`):
  a script of yours can use the same fake-command trick to run without a network.
