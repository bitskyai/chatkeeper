#!/usr/bin/env bash
# Packages dist/ChatKeeper.app as ChatKeeper-<VERSION>.zip (Sparkle update
# archive) and ChatKeeper-<VERSION>.dmg (drag-to-Applications installer).
#
#   VERSION            required, without the leading "v"
#   CODESIGN_IDENTITY  "-" (default, ad-hoc) or a Developer ID identity
#   NOTARIZE=1         submit to Apple notary service and staple; needs
#                      APPLE_ID, APPLE_TEAM_ID, APPLE_APP_PASSWORD
set -euo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$HERE/dist"
APP="$DIST/ChatKeeper.app"
: "${VERSION:?set VERSION, e.g. 0.1.0}"
IDENTITY="${CODESIGN_IDENTITY:--}"
ZIP="$DIST/ChatKeeper-$VERSION.zip"
DMG="$DIST/ChatKeeper-$VERSION.dmg"

[ -d "$APP" ] || { echo "missing $APP; run scripts/build-app.sh" >&2; exit 1; }

notarize() {
  xcrun notarytool submit "$1" --apple-id "$APPLE_ID" --team-id "$APPLE_TEAM_ID" \
    --password "$APPLE_APP_PASSWORD" --wait
}

if [ "${NOTARIZE:-0}" = "1" ]; then
  [ "$IDENTITY" != "-" ] || { echo "notarization requires a Developer ID identity" >&2; exit 1; }
  ditto -c -k --keepParent "$APP" "$DIST/notarize.zip"
  notarize "$DIST/notarize.zip"
  rm -f "$DIST/notarize.zip"
  xcrun stapler staple "$APP"
fi

rm -f "$ZIP" "$DMG"
ditto -c -k --sequesterRsrc --keepParent "$APP" "$ZIP"

STAGE="$DIST/dmg"
rm -rf "$STAGE"
mkdir -p "$STAGE"
ditto "$APP" "$STAGE/ChatKeeper.app"
ln -s /Applications "$STAGE/Applications"
if [ "$IDENTITY" = "-" ]; then
  cat > "$STAGE/READ ME FIRST.txt" <<'TXT'
ChatKeeper is not yet signed with an Apple Developer ID.

1. Drag ChatKeeper to Applications.
2. In Applications, right-click (or Control-click) ChatKeeper and choose Open,
   then click Open in the dialog. You only need to do this once.

On macOS 15 or later, if there is no Open button: open System Settings >
Privacy & Security, scroll down and click "Open Anyway" for ChatKeeper.

Or, in Terminal:
    xattr -dr com.apple.quarantine /Applications/ChatKeeper.app
TXT
fi
hdiutil create -volname "ChatKeeper $VERSION" -srcfolder "$STAGE" -ov -format UDZO "$DMG"
rm -rf "$STAGE"

if [ "$IDENTITY" != "-" ]; then
  codesign --force --sign "$IDENTITY" --timestamp "$DMG"
  if [ "${NOTARIZE:-0}" = "1" ]; then
    notarize "$DMG"
    xcrun stapler staple "$DMG"
  fi
fi
hdiutil verify "$DMG"
(cd "$DIST" && shasum -a 256 "$(basename "$ZIP")" "$(basename "$DMG")" | tee "ChatKeeper-$VERSION.sha256")
