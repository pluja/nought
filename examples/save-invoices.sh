#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# save-invoices.sh: save the attachments of your receipts to a folder,
# once each: a message whose files were saved is tagged "saved".
#
#   save-invoices.sh [dir]   # default ~/Documents/Invoices
#
#   NOU_SAVE_QUERY  the mail to save from (default "tag:receipts -tag:saved");
#                   ai-classify.sh can tag receipts for you
#
# An existing file is never overwritten: a second invoice.pdf is saved as
# "invoice (1).pdf".
#
# Needs: nou, jq.
set -euf # -f: no globbing, as queries may hold *
export NOU_PERM=tag

dir=${1:-$HOME/Documents/Invoices}
query=${NOU_SAVE_QUERY:-tag:receipts -tag:saved}

while :; do
	# shellcheck disable=SC2086 # the query's terms, split on purpose
	ids=$(nou search --limit 50 --json -- $query | jq -r '.messages[].id')
	[ -n "$ids" ] || break
	for id in $ids; do
		files=$(nou msg show "$id" --json | jq '.attachments | length')
		if [ "$files" -gt 0 ]; then
			nou msg save "$id" --dir "$dir" --json | jq -r '.[].path'
		fi
		nou msg tag "$id" --add saved >/dev/null
	done
done
