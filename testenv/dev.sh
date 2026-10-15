#!/bin/sh
# Runs the dev build of nou against the test environment only:
#   testenv/dev.sh search tag:inbox
here="$(cd "$(dirname "$0")" && pwd)"
export NOU_DEV=1 NOUGHT_HOME="$here/home" NOU_DEV_CA="$here/certs/tls.crt"
exec "$here/nou" "$@"
