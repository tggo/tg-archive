#!/bin/bash
# Збирає universal-бінарник, підписує Developer ID, нотарізує в Apple, пакує для Homebrew.
#
#   ./build/release.sh v0.1.0
#
# Потрібне оточення:
#   DEVELOPER_ID   — "Developer ID Application: Ім'я (TEAMID)" (за замовчуванням береться з keychain)
#   NOTARY_PROFILE — профіль notarytool (створюється `xcrun notarytool store-credentials`)
set -euo pipefail

VERSION="${1:?вкажи версію, напр. v0.1.0}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$ROOT/dist"
NAME="tg-archive"
DEVELOPER_ID="${DEVELOPER_ID:-$(security find-identity -v -p codesigning | awk -F'"' '/Developer ID Application/{print $2; exit}')}"
NOTARY_PROFILE="${NOTARY_PROFILE:-tg-archive-notary}"

[ -n "$DEVELOPER_ID" ] || { echo "не знайдено Developer ID Application у keychain" >&2; exit 1; }
echo "==> підпис: $DEVELOPER_ID"

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

echo "==> notarize"
# нотарізують архів, а не голий бінарник
ditto -c -k --keepParent "$DIST/$NAME" "$DIST/$NAME-notarize.zip"
if xcrun notarytool submit "$DIST/$NAME-notarize.zip" \
     --keychain-profile "$NOTARY_PROFILE" --wait; then
  # stapler не вміє клеїти квиток до голого CLI-бінарника — перевіряємо через spctl,
  # Gatekeeper однаково спитає Apple онлайн і побачить нотарізацію.
  echo "==> перевірка Gatekeeper"
  spctl -a -vvv -t install "$DIST/$NAME" 2>&1 | sed 's/^/    /' || true
else
  echo "!! нотарізація не пройшла — див. лог вище" >&2
  exit 1
fi
rm "$DIST/$NAME-notarize.zip"

echo "==> tarball для Homebrew"
TAR="$DIST/${NAME}_${VERSION#v}_darwin_universal.tar.gz"
tar -czf "$TAR" -C "$DIST" "$NAME"
shasum -a 256 "$TAR" | tee "$TAR.sha256"

echo
echo "готово: $TAR"
echo "sha256: $(awk '{print $1}' "$TAR.sha256")"
