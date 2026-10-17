#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# notify.sh: sync, and show a desktop notification when new mail arrived.
#
# Run it instead of "nou sync", from cron or a systemd timer. It shows the
# count only: senders and subjects would stay in the notification history.
#
#   NOU_NOTIFY_COMMAND  run as: $NOU_NOTIFY_COMMAND <title> <text>
#                       (default: notify-send, or osascript on macOS)
#
# It exits as nou sync did (3: some folders failed; see nou sync).
#
# Needs: nou, jq.
set -euf # -f: no globbing, as queries may hold *

status=0
out=$(nou sync --json) || status=$?
new=$(printf '%s' "$out" | jq '[.accounts[]?.folders[]? | select(.folder | ascii_downcase == "inbox") | .fetched] | add // 0')

if [ "$new" -gt 0 ]; then
	text="$new new message"
	[ "$new" -eq 1 ] || text="${text}s"
	if [ -n "${NOU_NOTIFY_COMMAND:-}" ]; then
		# shellcheck disable=SC2086 # a command line, split into words on purpose
		$NOU_NOTIFY_COMMAND nought "$text"
	elif command -v notify-send >/dev/null; then
		notify-send nought "$text"
	elif command -v osascript >/dev/null; then
		osascript -e "display notification \"$text\" with title \"nought\""
	fi
fi
exit "$status"
