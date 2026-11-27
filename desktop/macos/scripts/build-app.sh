#!/usr/bin/env bash
# Assembles dist/ChatKeeper.app (universal arm64 + x86_64) without Xcode projects.
#
# Environment (all optional):
#   VERSION              marketing version, e.g. 0.1.0 (default: git describe)
#   BUILD_NUMBER         monotonically increasing CFBundleVersion (default: 1)
#   CHATKEEPER_BIN       prebuilt universal chatkeeper binary (default: build with go)
#   CODESIGN_IDENTITY    "-" for ad-hoc (default) or "Developer ID Application: …"
#   SPARKLE_PUBLIC_ED_KEY  EdDSA public key; empty disables in-app updates
#   SPARKLE_FEED_URL     appcast URL (default: latest GitHub release appcast.xml)
#   BUNDLE_ID            default ai.bitsky.chatkeeper
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
DIST="$HERE/dist"
APP="$DIST/ChatKeeper.app"
WORK="$DIST/work"

VERSION="${VERSION:-$(git -C "$ROOT" describe --tags --always 2>/dev/null | sed 's/^v//' || echo 0.0.0-dev)}"
BUILD_NUMBER="${BUILD_NUMBER:-1}"
CODESIGN_IDENTITY="${CODESIGN_IDENTITY:--}"
SPARKLE_PUBLIC_ED_KEY="${SPARKLE_PUBLIC_ED_KEY:-}"
SPARKLE_FEED_URL="${SPARKLE_FEED_URL:-https://github.com/bitskyai/chatkeeper/releases/latest/download/appcast.xml}"
BUNDLE_ID="${BUNDLE_ID:-ai.bitsky.chatkeeper}"

rm -rf "$APP" "$WORK"
mkdir -p "$WORK" "$APP/Contents/MacOS" "$APP/Contents/Resources" "$APP/Contents/Frameworks"

echo "==> Swift menu-bar app ($VERSION, build $BUILD_NUMBER)"
cd "$HERE"
slices=()
for triple in arm64-apple-macosx13.0 x86_64-apple-macosx13.0; do
  swift build -c release --product ChatKeeper --triple "$triple"
  slices+=("$(swift build -c release --product ChatKeeper --triple "$triple" --show-bin-path)/ChatKeeper")
done
lipo -create "${slices[@]}" -output "$APP/Contents/MacOS/ChatKeeper"

echo "==> Sparkle.framework"
SPARKLE="$(find "$HERE/.build/artifacts" -type d -path '*Sparkle.xcframework/macos-*/Sparkle.framework' -prune | head -n1)"
[ -n "$SPARKLE" ] || { echo "Sparkle.framework not found in .build/artifacts" >&2; exit 1; }
ditto "$SPARKLE" "$APP/Contents/Frameworks/Sparkle.framework"

echo "==> chatkeeper server binary (universal)"
if [ -n "${CHATKEEPER_BIN:-}" ]; then
  cp "$CHATKEEPER_BIN" "$WORK/chatkeeper"
else
  COMMIT="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  for arch in arm64 amd64; do
    (cd "$ROOT" && CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" \
      -o "$WORK/chatkeeper-$arch" ./cmd/chatkeeper)
  done
  lipo -create "$WORK/chatkeeper-arm64" "$WORK/chatkeeper-amd64" -output "$WORK/chatkeeper"
fi
install -m 0755 "$WORK/chatkeeper" "$APP/Contents/Resources/chatkeeper"

echo "==> Info.plist and icon"
sed -e "s|@BUNDLE_ID@|$BUNDLE_ID|" -e "s|@VERSION@|$VERSION|" -e "s|@BUILD@|$BUILD_NUMBER|" \
    -e "s|@FEED_URL@|$SPARKLE_FEED_URL|" -e "s|@ED_PUBLIC_KEY@|$SPARKLE_PUBLIC_ED_KEY|" \
    "$HERE/Resources/Info.plist" > "$APP/Contents/Info.plist"
plutil -lint "$APP/Contents/Info.plist"
printf 'APPL????' > "$APP/Contents/PkgInfo"

swift "$HERE/scripts/make-icon.swift" "$WORK/icon.png"
ICONSET="$WORK/AppIcon.iconset"
mkdir -p "$ICONSET"
for size in 16 32 128 256 512; do
  sips -z $size $size "$WORK/icon.png" --out "$ICONSET/icon_${size}x${size}.png" >/dev/null
  sips -z $((size * 2)) $((size * 2)) "$WORK/icon.png" --out "$ICONSET/icon_${size}x${size}@2x.png" >/dev/null
done
iconutil -c icns "$ICONSET" -o "$APP/Contents/Resources/AppIcon.icns"

"$HERE/scripts/sign.sh" "$APP"

echo "==> Verify"
for bin in "$APP/Contents/MacOS/ChatKeeper" "$APP/Contents/Resources/chatkeeper"; do
  archs="$(lipo -archs "$bin")"
  echo "$bin: $archs"
  case "$archs" in *arm64*x86_64*|*x86_64*arm64*) ;; *) echo "not universal: $bin" >&2; exit 1 ;; esac
done
"$APP/Contents/Resources/chatkeeper" version
echo "Built $APP"
