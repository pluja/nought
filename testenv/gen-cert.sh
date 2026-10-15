#!/bin/sh
# Generates a throwaway self-signed certificate for the test server, valid
# for localhost and 127.0.0.1. nou trusts it via NOU_TEST_CA (dev only).
set -eu
dir="$(dirname "$0")/certs"
[ -f "$dir/tls.crt" ] && exit 0
mkdir -p "$dir"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3650 \
  -subj "/CN=nought-testenv" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
  -keyout "$dir/tls.key" -out "$dir/tls.crt" 2>/dev/null
chmod 644 "$dir/tls.key" "$dir/tls.crt"   # readable by the container's vmail user
echo "generated $dir/tls.crt"
