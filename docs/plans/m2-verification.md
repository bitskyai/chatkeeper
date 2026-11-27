# M2 verification and implementation notes

Issue #2's browser flow is exercised in desktop and server modes by
`TestBrowserWizard`: a clean application home, server setup code/password,
registry cards, prefilled manifest, invalid bot token, missing named scopes,
polling-only notice, channels (including bot-not-in-channel), trigger, storage,
a fake Socket Mode reaction, persisted capture event, and verified viewer.
Screenshots in `docs/screenshots/` contain synthetic fixtures only; the storage
screenshot uses a generic display path. Tests never contact the real chat API.

Security coverage includes every administrative route without a session, missing
and wrong CSRF tokens, hostile Origin and Host, bootstrap after database loss,
invalid upgraded configuration, public desktop bind rejection, login throttling,
idle session expiry, environment credential handling, secret-free wizard state,
private secret files, path traversal and symlinks. Runtime tests cover additions,
removals, settings changes, isolated credential rotation, invalid retention and
ConfigMap-style symlink swaps. Nested YAML comments and ordering survive updates;
the CLI consumes the same persisted credentials as the UI.

Dependencies: modernc.org/sqlite, zalando/go-keyring, x/crypto (argon2id), x/sys
(Windows instance locks and ACLs), chromedp (tests), and vendored htmx 2.0.8.
SQLite uses database/sql, embedded numbered SQL migrations, WAL and one writer.

Implementation decisions:
- Changed workspaces restart in isolation; unchanged workspaces keep their
  connections. Opportunistic in-place Session.Reconfigure is deferred (the
  current implemented platform always declines it).
- Pause flags are app state in SQLite and survive restarts. Removing a workspace
  clears the flag and leaves archives intact.
- A private backend metadata file pins a credential's selected backend so an
  unavailable/recovered keychain cannot resurrect a superseded value. Environment
  still takes precedence. OS keychain accounts are scoped to the application home.
- Wizard credentials are intentionally memory-only; restarting the process requires
  restarting unfinished setup. Non-secret progress is recorded in SQLite.
- The mention scan part of #11 is fixed and has a concurrency regression test.
  Same-ID attachment hash conflicts still fail closed and can stall refresh; #11
  remains open. Archive blob versioning is outside this milestone.
- Search is M3; cleanup is M5. No placeholder search or destructive cleanup UI.

Native OS keychain and Windows ACL behavior still need smoke tests on their
respective desktop platforms; all six pure-Go target builds are covered by CI.
