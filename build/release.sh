#!/bin/bash
# Builds a universal binary, signs it with a Developer ID, notarizes it with Apple,
# and packs the tarball the Homebrew formula points at.
#
#   ./build/release.sh v0.1.0
#
# Environment:
#   DEVELOPER_ID   - "Developer ID Application: Name (TEAMID)"; taken from the keychain by default
#   NOTARY_PROFILE - notarytool profile (see build/notary-setup.md). Missing profile means
#                    the build is still signed, just not notarized.
set -euo pipefail

VERSION="${1:?pass a version, e.g. v0.1.0}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
NAME="tg-archive"
DEVELOPER_ID="${DEVELOPER_ID:-$(security find-identity -v -p codesigning | awk -F'"' '/Developer ID Application/{print $2; exit}')}"
NOTARY_PROFILE="${NOTARY_PROFILE:-tg-archive-notary}"

[ -n "$DEVELOPER_ID" ] || { echo "no Developer ID Application identity in the keychain" >&2; exit 1; }
echo "==> signing identity: $DEVELOPER_ID"

rm -rf "$DIST"; mkdir -p "$DIST"
cd "$ROOT"

LDFLAGS="-s -w -X main.version=${VERSION#v}"
for arch in arm64 amd64; do
  echo "==> build darwin/$arch"
  CGO_ENABLED=0 GOOS=darwin GOARCH=$arch \
    go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/$NAME-$arch" ./cmd/$NAME
done

echo "==> universal binary"
lipo -create -output "$DIST/$NAME" "$DIST/$NAME-arm64" "$DIST/$NAME-amd64"
rm "$DIST/$NAME-arm64" "$DIST/$NAME-amd64"
lipo -info "$DIST/$NAME"

echo "==> codesign (hardened runtime, secure timestamp)"
codesign --force --options runtime --timestamp \
  --sign "$DEVELOPER_ID" \
  --identifier "com.tggo.tg-archive" \
  "$DIST/$NAME"
codesign --verify --strict --verbose=2 "$DIST/$NAME"

if xcrun notarytool history --keychain-profile "$NOTARY_PROFILE" >/dev/null 2>&1; then
  echo "==> notarize"
  # Apple notarizes an archive, not a bare binary
  ditto -c -k --keepParent "$DIST/$NAME" "$DIST/$NAME-notarize.zip"
  xcrun notarytool submit "$DIST/$NAME-notarize.zip" --keychain-profile "$NOTARY_PROFILE" --wait
  rm "$DIST/$NAME-notarize.zip"
  # A bare CLI binary cannot carry a stapled ticket; Gatekeeper checks with Apple online.
  echo "==> Gatekeeper check"
  spctl -a -vvv -t install "$DIST/$NAME" 2>&1 | sed 's/^/    /' || true
else
  echo "!! no notary profile \"$NOTARY_PROFILE\" — releasing SIGNED BUT NOT NOTARIZED" >&2
  echo "   set one up with build/notary-setup.md, then re-run to publish a notarized build" >&2
fi

echo "==> tarball for Homebrew"
TAR="$DIST/${NAME}_${VERSION#v}_darwin_universal.tar.gz"
tar -czf "$TAR" -C "$DIST" "$NAME"
shasum -a 256 "$TAR" | tee "$TAR.sha256"

# Linux builds: same pure-Go binary, no signing involved. Useful for running `live` on a
# server or in a container.
for arch in arm64 amd64; do
  echo "==> build linux/$arch"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch \
    go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/$NAME-linux-$arch" ./cmd/$NAME
  LTAR="$DIST/${NAME}_${VERSION#v}_linux_${arch}.tar.gz"
  mv "$DIST/$NAME-linux-$arch" "$DIST/$NAME"
  tar -czf "$LTAR" -C "$DIST" "$NAME"
  shasum -a 256 "$LTAR" | tee "$LTAR.sha256"
  rm "$DIST/$NAME"
done
# restore the signed universal binary that the tarball above was made from
tar -xzf "$TAR" -C "$DIST"

echo
echo "done:"
ls -1 "$DIST"/*.tar.gz | sed 's/^/  /'
echo "darwin sha256: $(awk '{print $1}' "$TAR.sha256")"
