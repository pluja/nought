#!/bin/sh
# Builds the release archives into dist/: nou for every supported system,
# with README and LICENSE, and SHA256SUMS. The build is reproducible: run it
# twice on the same commit and the checksums match. Needs GNU tar and
# coreutils (Linux, as in CI) and zip.
#   tools/dist.sh v0.1.0
set -eu
version="${1:?usage: tools/dist.sh VERSION}"
cd "$(dirname "$0")/.."
# Every file date is the commit's, so archives do not depend on when or
# where they were built.
SOURCE_DATE_EPOCH="$(git log -1 --format=%ct)"
# The toolchain go.mod names, never a distribution's patched one (Fedora's
# sets GOEXPERIMENT), so a rebuild anywhere gives the same bytes as CI.
GOTOOLCHAIN="$(sed -n 's/^toolchain //p' go.mod)"
export SOURCE_DATE_EPOCH CGO_ENABLED=0 GOTOOLCHAIN

rm -rf dist
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64 freebsd/amd64 freebsd/arm64; do
	os="${target%/*}" arch="${target#*/}"
	name="nou_${version}_${os}_${arch}"
	dir="dist/$name"
	bin=nou
	[ "$os" = windows ] && bin=nou.exe
	mkdir -p "$dir"
	GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$dir/$bin" ./cmd/nou
	cp README.md LICENSE "$dir/"
	touch -d "@$SOURCE_DATE_EPOCH" "$dir" "$dir"/*
	if [ "$os" = windows ]; then
		(cd dist && zip -qX -r "$name.zip" "$name")
	else
		tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner \
			-C dist -cf - "$name" | gzip -n -9 >"dist/$name.tar.gz"
	fi
	rm -rf "$dir"
done
(cd dist && sha256sum -- * >SHA256SUMS)
ls -l dist
