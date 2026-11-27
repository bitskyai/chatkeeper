#!/usr/bin/env bash
# Signs ChatKeeper.app inside-out. CODESIGN_IDENTITY="-" (default) ad-hoc signs
# so the app runs on Apple silicon without a Developer ID. Set it to a
# "Developer ID Application: …" identity to enable hardened runtime and secure
# timestamps for notarization (see desktop/macos/README.md).
set -euo pipefail

APP="$1"
IDENTITY="${CODESIGN_IDENTITY:--}"
ENTITLEMENTS="$(cd "$(dirname "$0")/.." && pwd)/Resources/ChatKeeper.entitlements"

flags=(--force --sign "$IDENTITY")
if [ "$IDENTITY" = "-" ]; then
  flags+=(--timestamp=none)
  echo "==> Ad-hoc signing (unsigned distribution)"
else
  flags+=(--timestamp --options runtime)
  echo "==> Signing with: $IDENTITY"
fi

FW="$APP/Contents/Frameworks/Sparkle.framework"
if [ -d "$FW" ]; then
  # Sparkle's documented order: XPC services, helpers, then the framework.
  find "$FW/Versions/B/XPCServices" -maxdepth 1 -name '*.xpc' -print0 2>/dev/null |
    while IFS= read -r -d '' xpc; do codesign "${flags[@]}" "$xpc"; done
  [ -e "$FW/Versions/B/Autoupdate" ] && codesign "${flags[@]}" "$FW/Versions/B/Autoupdate"
  [ -d "$FW/Versions/B/Updater.app" ] && codesign "${flags[@]}" "$FW/Versions/B/Updater.app"
  codesign "${flags[@]}" "$FW"
fi
codesign "${flags[@]}" "$APP/Contents/Resources/chatkeeper"
codesign "${flags[@]}" --entitlements "$ENTITLEMENTS" "$APP"
codesign --verify --deep --strict --verbose=2 "$APP"
