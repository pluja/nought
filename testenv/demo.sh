#!/bin/sh
# Records docs/images/demo.gif: the real nou over a made-up mailbox
# (internal/demo), entirely in the test environment. Start it first with
# make testenv-up. It never touches your real mail: NOU_DEV=1, NOUGHT_HOME is
# testenv/demo-home, mail comes from the test Dovecot on 127.0.0.1 and is
# sent to a local SMTP sink that throws it away.
set -eu
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(dirname "$here")"
export NOU_DEV=1 NOUGHT_HOME="$here/demo-home" NOU_DEV_CA="$here/certs/tls.crt" DEMO_BIN="$here/demo-bin"
cd "$repo"

mkdir -p "$DEMO_BIN"
CGO_ENABLED=0 go build -trimpath -o "$DEMO_BIN/nou" ./cmd/nou
go build -o "$DEMO_BIN/smtpsink" ./testenv/smtpsink

# A fresh mailbox each time: any user name works on the test Dovecot.
rm -rf "$NOUGHT_HOME"
user="demo-$(date +%s)@nought.test"
go run ./testenv/seed -demo "$user"
nou() { "$DEMO_BIN/nou" "$@" >/dev/null; }
nou account add personal --email sam@example.org --username "$user" --imap-host 127.0.0.1 --imap-port 10993 \
	--password-command 'echo nought-test-password' --smtp-host 127.0.0.1 --smtp-port 10465
nou account set personal --deletion enabled
sed -i '1i send_delay = "6s"' "$NOUGHT_HOME/config/config.toml"
nou sync
nou msg tag subject:lisbon --add travel
nou msg tag subject:invoice --add bills --add flagged
nou msg delete subject:"sign-in code"

"$DEMO_BIN/smtpsink" 2>"$here/demo-home/smtpsink.log" &
sink=$!
trap 'kill $sink' EXIT
vhs "$here/demo.tape"
