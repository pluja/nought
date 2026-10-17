#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# backup.sh: back up what nou paths recommends, and nothing else: the
# archive, the tag dump, the config, drafts, the outbox and the expunge
# journal. Never the trash or expiring mail: you asked for it to go. The
# index is left out too; nou reindex rebuilds it.
#
#   backup.sh restic           # restic backup (RESTIC_REPOSITORY etc. set as usual)
#   backup.sh rsync <dest>     # a copy under <dest>, full paths kept
#
# The tag dump is rewritten after every tag change, so it is always current.
#
# Needs: nou, jq, and restic or rsync.
set -euf # -f: no globbing, as queries may hold *
export NOU_PERM=read

usage() {
	echo "usage: backup.sh restic | backup.sh rsync <dest>" >&2
	exit 2
}

# Only what exists: a new setup has no drafts or outbox yet.
paths=$(nou paths --json | jq -r '.backup.include[]' | while IFS= read -r p; do
	if [ -e "$p" ]; then printf "%s\n" "$p"; fi
done)
[ -n "$paths" ] || {
	echo "backup.sh: nothing to back up yet" >&2
	exit 1
}

case ${1:-} in
restic)
	printf '%s\n' "$paths" | restic backup --tag nought --files-from-verbatim /dev/stdin
	;;
rsync)
	[ $# -eq 2 ] || usage
	printf '%s\n' "$paths" | rsync -a -r --files-from=- / "$2"
	;;
*) usage ;;
esac
