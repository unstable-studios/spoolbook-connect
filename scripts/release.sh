#!/bin/sh
# Builds the release Connect installs: one directory with module.yaml at its
# root, one binary per platform named <name>_<GOOS>_<GOARCH>, and a
# checksums.txt listing every file. Then packs it as a .tar.gz, which
# `prusa-connect module add -from` also takes.
#
#   sh scripts/release.sh [out-dir]
set -eu

OUT=${1:-release}
NAME=$(sed -n 's/^name:[[:space:]]*//p' module.yaml | head -1)
VERSION=$(sed -n 's/^version:[[:space:]]*\([0-9][^[:space:]#]*\).*/\1/p' module.yaml | head -1)
# The platforms module.yaml lists, one GOOS_GOARCH per line.
PLATFORMS=$(awk '
	/^platforms:/ { inside = 1; next }
	inside && /^[^ \t#]/ { inside = 0 }
	inside && /^[ \t]*-[ \t]/ { sub(/^[ \t]*-[ \t]*/, ""); sub(/[ \t]*(#.*)?$/, ""); print }
' module.yaml)

rm -rf "$OUT"
mkdir -p "$OUT"
cp module.yaml openapi.json LICENSE README.md "$OUT/"

for platform in $PLATFORMS; do
	goos=${platform%%_*}
	goarch=${platform#*_}
	binary=${NAME}_$platform
	[ "$goos" = windows ] && binary=$binary.exe
	echo "building $binary"
	CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -ldflags='-s -w' -o "$OUT/$binary" .
done

(cd "$OUT" && sha256sum -- * > checksums.txt)

# entries by their own names: Connect refuses an archive holding "./"
ARCHIVE=$PWD/spoolbook-connect_$VERSION.tar.gz
(cd "$OUT" && tar -czf "$ARCHIVE" -- *)
echo "$ARCHIVE"
