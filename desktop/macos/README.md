# ChatKeeper for macOS (menu-bar app)

A SwiftUI `MenuBarExtra` app (macOS 13+, no Dock icon) that bundles the
universal `chatkeeper` binary in `ChatKeeper.app/Contents/Resources` and runs it
as a child process:

```
chatkeeper run --mode desktop --no-browser --bind 127.0.0.1:8787
```

The app reads the per-launch login URL from the child's stdout
(`Open ChatKeeper: http://127.0.0.1:PORT/login?token=…`). If port 8787 is busy
the server picks an ephemeral port and prints that URL instead. The token is used
for three things:

- **Open ChatKeeper** opens the login URL. Recent archives use `&next=/view/…`.
- **Status** polls `GET /healthz` and `GET /api/desktop/status` (bearer = launch
  token) every 5 s. The JSON has workspace ids, states, error classes and
  timestamps, plus the last five archived threads. It never contains
  credentials, settings, paths or message content.
- **Pause/Resume** sends `POST /api/desktop/pause {"paused": true|false}`.

The `/api/desktop/*` endpoints exist only in desktop mode and accept only the
launch token, never a browser cookie.

**Single instance.** The app checks its bundle id at launch. The server keeps
the CLI's `instance.lock` in `~/Library/Application Support/chatkeeper`. If the
CLI or `brew services` already holds the lock, the child exits with "already
running" and the app attaches to that instance via `instance.url`. Quit leaves an
attached instance running and stops the app's own child (SIGTERM, then SIGKILL
after 10 s). If the child exits unexpectedly, the app restarts it with
exponential backoff and gives up after 5 exits in 5 minutes.

Child stderr and stdout are logged to `~/Library/Logs/ChatKeeper/chatkeeper.log`
with tokens redacted. **Show Logs** opens that file.

## Layout

| Path | Purpose |
| --- | --- |
| `Sources/ChatKeeperKit` | Foundation-only logic: launch URL parsing, status model, indicator mapping, restart policy. Unit tested, and also builds on Linux. |
| `Sources/ChatKeeper` | AppKit/SwiftUI app: menu, process supervisor, Sparkle, `SMAppService` login item. |
| `Resources/Info.plist` | Template; `build-app.sh` fills version, bundle id and Sparkle keys. `LSUIElement` hides the Dock icon. |
| `scripts/make-icon.swift` | Draws the app icon with CoreGraphics. No binary artwork is committed. |
| `scripts/build-app.sh` | Builds universal `dist/ChatKeeper.app`: Swift per-arch builds plus `lipo`, the Go binary for darwin arm64+amd64 plus `lipo`, Sparkle.framework, icon and signing. |
| `scripts/sign.sh` | Signs inside-out. Ad-hoc by default. |
| `scripts/package.sh` | Builds `ChatKeeper-VERSION.zip` (with `ditto`, the Sparkle update archive), `.dmg` (with `hdiutil`) and `.sha256`. Notarization is optional. |
| `scripts/make-appcast.py` | Writes a Sparkle appcast from `sign_update` output. |
| `scripts/make-cask.py` | Writes the `chatkeeper-app` Homebrew cask for a DMG. |

## Build locally (on a Mac with Xcode or the Command Line Tools, plus Go)

```sh
cd desktop/macos
swift test                                     # ChatKeeperKit unit tests
VERSION=0.1.0 scripts/build-app.sh             # dist/ChatKeeper.app
VERSION=0.1.0 scripts/package.sh               # dist/ChatKeeper-0.1.0.{dmg,zip,sha256}
open dist/ChatKeeper.app
```

To test with another port, run `defaults write ai.bitsky.chatkeeper ServerPort -int 9876`.

## CI and releases

`.github/workflows/desktop.yml` runs on `macos-latest`:

- **PRs** touching `desktop/`, `cmd/chatkeeper/` or `internal/web/desktop*.go`
  run these steps:
  - `swift test`
  - build the universal app and package it
  - render the cask and a sample appcast
  - smoke-test the bundled server's status endpoint
  - launch the real app, check that it started its server, quit it, and check
    that the child stopped cleanly
  - upload the DMG and zip as workflow artifacts

  These jobs use no secrets.
- **`v*` tags** (environment `release`) run these steps:
  - rebuild
  - wait for the GitHub Release that `release.yml` (GoReleaser) creates
  - upload `ChatKeeper-VERSION.dmg`, `.zip`, `.sha256` and `appcast.xml`
  - append install notes
  - push `Casks/chatkeeper-app.rb` to `bitskyai/homebrew-tap`

### Configuration switches (repository or `release` environment)

| Name | Kind | Effect when set |
| --- | --- | --- |
| `HOMEBREW_TAP_TOKEN` | secret | Fine-grained PAT with **Contents: read and write** on `bitskyai/homebrew-tap`. GoReleaser uses it to publish `Formula/chatkeeper.rb`, and this workflow uses it to publish `Casks/chatkeeper-app.rb`. When it is unset, both are skipped. |
| `SPARKLE_PUBLIC_ED_KEY` | variable | Embedded as `SUPublicEDKey`. This enables in-app updates. Without it, **Check for Updates…** opens the Releases page. |
| `SPARKLE_ED_PRIVATE_KEY` | secret | Signs the update zip and publishes `appcast.xml`. |
| `MACOS_CERTIFICATE_P12` / `MACOS_CERTIFICATE_PASSWORD` | secrets | Base64 Developer ID Application certificate (.p12) and its password. |
| `MACOS_CODESIGN_IDENTITY` | variable | For example `Developer ID Application: Example (TEAMID)`. With the certificate, this switches from ad-hoc signing to Developer ID signing with the hardened runtime. |
| `APPLE_ID` / `APPLE_TEAM_ID` / `APPLE_APP_PASSWORD` | secrets | Enable `notarytool` notarization and stapling of the app and DMG. |

With none of these set, the release is **unsigned**: every Mach-O is ad-hoc
signed with `codesign -s -`, so it runs on Apple silicon. The cask removes the
quarantine attribute in `postflight`, and the release notes explain right-click
→ Open and `xattr`.

### Sparkle EdDSA keys

The appcast lives at
`https://github.com/bitskyai/chatkeeper/releases/latest/download/appcast.xml`.
GitHub's `latest` alias skips prereleases, so only stable tags update existing
installs. Sparkle compares `CFBundleVersion`, which is the workflow run number
and always increases.

Generate the key pair once, on a Mac. **Never commit the private key.**

```sh
cd desktop/macos && swift package resolve
.build/artifacts/sparkle/Sparkle/bin/generate_keys            # stores the private key in your login Keychain, prints the public key
.build/artifacts/sparkle/Sparkle/bin/generate_keys -x sparkle_private.key   # export it for CI
gh variable set SPARKLE_PUBLIC_ED_KEY --env release --body '<printed public key>'
gh secret set SPARKLE_ED_PRIVATE_KEY --env release < sparkle_private.key
rm -P sparkle_private.key
```

Keep a backup of the private key. If you lose it, existing installs can no
longer verify updates.

### Signing and notarization later (Apple Developer account)

1. Create a **Developer ID Application** certificate. Export it with its
   private key as a `.p12` file.
2. Store the certificate and identity:
   ```sh
   base64 -i cert.p12 | gh secret set MACOS_CERTIFICATE_P12 --env release
   gh secret set MACOS_CERTIFICATE_PASSWORD --env release
   gh variable set MACOS_CODESIGN_IDENTITY --env release --body 'Developer ID Application: …'
   ```
3. Create an app-specific password at appleid.apple.com. Store it as
   `APPLE_APP_PASSWORD`, along with `APPLE_ID` and `APPLE_TEAM_ID`.
4. Tag a release. The workflow then signs with the hardened runtime, notarizes,
   staples, and drops the unsigned notes and the cask's quarantine workaround.

Locally, the same switch is
`CODESIGN_IDENTITY='Developer ID Application: …' NOTARIZE=1 APPLE_ID=… APPLE_TEAM_ID=… APPLE_APP_PASSWORD=… scripts/package.sh`
after running `build-app.sh` with the same `CODESIGN_IDENTITY`.
