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
- **Pause/Resume** sends `POST /api/desktop/pause {"paused": true|false}`. It
  sets that state on every workspace in one reload and one database
  transaction, so the call is idempotent and never leaves a partial pause.

The `/api/desktop/*` endpoints exist only in desktop mode and accept only the
launch token, never a browser cookie.

**Single instance.** The app checks its bundle id at launch. The server keeps
the CLI's `instance.lock` in `~/Library/Application Support/chatkeeper`. If the
CLI or `brew services` already holds the lock, the child exits with "already
running" and the app attaches to that instance via `instance.url`. Quit leaves an
attached instance running and stops the app's own child (SIGTERM, then SIGKILL
after 10 s). If the child exits unexpectedly, the app restarts it with
exponential backoff and gives up after 5 exits in 5 minutes.

**No orphaned server.** The child runs in *launcher mode*
(`CHATKEEPER_LAUNCHER=menubar`, `CHATKEEPER_LAUNCHER_PID=<app pid>`): its stdin
is a pipe whose write end only the app holds, and the server shuts down
gracefully on stdin EOF or when its parent pid changes. Both happen when the app
crashes or is force-quit. The app also records the child it owns
(pid, executable path and start time) in `desktop-child.json` in the data
folder; on the next launch it stops that exact process if it is still running
before starting a new one, instead of attaching to it as an external instance.

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
  - SIGKILL the app and check that its server exits by itself, then relaunch
    and quit again
  - upload the DMG and zip as workflow artifacts

  These jobs use no secrets. Sparkle is pinned by commit in `Package.swift`
  and `Package.resolved`; builds use `--force-resolved-versions`.
- **`v*` tags** (environment `release`) run these steps:
  - rebuild
  - wait for the GitHub Release that `release.yml` (GoReleaser) creates
  - attest the DMG, zip and `.sha256` with GitHub build provenance
  - upload `ChatKeeper-VERSION.dmg`, `.zip`, `.sha256` and `appcast.xml`
  - append install notes
  - push `Casks/chatkeeper-app.rb` to `bitskyai/homebrew-tap` (stable tags
    only; prereleases such as `v0.1.0-rc.1` are never published to the tap)

  Each secret is passed only to the step that needs it.

### Configuration switches (repository or `release` environment)

| Name | Kind | Effect when set |
| --- | --- | --- |
| `HOMEBREW_TAP_TOKEN` | secret | Fine-grained PAT with **Contents: read and write** on `bitskyai/homebrew-tap`. `release.yml` uses it to publish `Formula/chatkeeper.rb` (rendered by `scripts/make-formula.py`), and this workflow uses it to publish `Casks/chatkeeper-app.rb`. It is sent as an HTTP header (`scripts/publish-tap.sh`), never in a URL. When it is unset, both are skipped. |
| `SPARKLE_PUBLIC_ED_KEY` | variable | Embedded as `SUPublicEDKey`. This enables in-app updates. Without it, **Check for Updates…** opens the Releases page. |
| `SPARKLE_ED_PRIVATE_KEY` | secret | Signs the update zip and publishes `appcast.xml`. |
| `MACOS_CERTIFICATE_P12` / `MACOS_CERTIFICATE_PASSWORD` | secrets | Base64 Developer ID Application certificate (.p12) and its password. |
| `MACOS_CODESIGN_IDENTITY` | variable | For example `Developer ID Application: Example (TEAMID)`. With the certificate, this switches from ad-hoc signing to Developer ID signing with the hardened runtime. |
| `APPLE_NOTARY_API_KEY` | secret | App Store Connect API key (`.p8` contents). With a Developer ID identity, enables `notarytool` notarization and stapling of the app and DMG. |
| `APPLE_NOTARY_API_KEY_ID` / `APPLE_NOTARY_API_ISSUER` | variables | Key ID and issuer ID of that API key. |

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
3. In App Store Connect → Users and Access → Integrations, create a Team API
   key with the Developer role and download `AuthKey_<ID>.p8`:
   ```sh
   gh secret set APPLE_NOTARY_API_KEY --env release < AuthKey_XXXXXXXXXX.p8
   gh variable set APPLE_NOTARY_API_KEY_ID --env release --body XXXXXXXXXX
   gh variable set APPLE_NOTARY_API_ISSUER --env release --body <issuer uuid>
   ```
   `package.sh` writes the key to a 0600 temp file for `notarytool`; no
   password is ever passed on a command line.
4. Tag a release. The workflow then signs with the hardened runtime, notarizes,
   staples, and drops the unsigned notes and the cask's quarantine workaround.

Locally, the same switch is
`CODESIGN_IDENTITY='Developer ID Application: …' NOTARIZE=1 NOTARY_KEYCHAIN_PROFILE=chatkeeper-notary scripts/package.sh`
after running `build-app.sh` with the same `CODESIGN_IDENTITY`. Create the
profile once, interactively, with `xcrun notarytool store-credentials chatkeeper-notary`
(it prompts for the password and stores it in your login Keychain).
