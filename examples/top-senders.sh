#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# top-senders.sh: who fills your inbox. Lists senders by how many messages
# they have, to clear the inbox in bulk:
#
#   top-senders.sh               # the inbox
#   top-senders.sh tag:unread    # any query
#   nou msg archive from:news@shop.example
#
# Needs: nou, jq.
set -euf # -f: no globbing, as queries may hold *
export NOU_PERM=read

[ $# -gt 0 ] || set -- tag:inbox
limit=1000
offset=0
{
	while :; do
		page=$(nou search --limit "$limit" --offset "$offset" --json -- "$@")
		printf '%s' "$page" | jq -r '.messages[].from'
		[ "$(printf '%s' "$page" | jq .more)" = true ] || break
		offset=$((offset + limit))
	done
} | sed 's/.*<\([^>]*\)>.*/\1/' | tr '[:upper:]' '[:lower:]' | sort | uniq -c | sort -rn
