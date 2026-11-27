# Slack message corpus and persistent logs implementation plan

Goal: archive unexpected source shapes without losing original responses and retain useful redacted diagnostics.

Architecture: keep transport responses as checksummed archive documents, use bounded best-effort text extraction, and report unrendered items in manifest gaps. Use a synchronized rotating JSON sink and a human stderr handler; expose authenticated bounded log reads through the UI and CLI. Extend existing macOS supervision logging.

Constraints: stack on fix/plugin-error-detail; no source-specific names in core; no private identifiers or credentials in committed fixtures; do not merge.

- [ ] Corpus: add sanitized synthetic API-shaped fixtures for subtypes, blocks, rich text, attachments, files and hostile/irregular values. Table tests exercise snapshot, persistence, viewer escaping and search extraction. Add corpus-seeded decoder fuzzing and run ten minutes.
- [ ] Robustness: retain exact API response bytes in raw documents; preserve partial text on extraction limits; record per-item unrendered notes without weakening archive path/integrity checks.
- [ ] Logs: rotation, redaction and level tests first, then wire structured server output, authenticated tail/filter/download UI and logs CLI.
- [ ] Desktop: rotate child output and provide redacted diagnostics copy alongside Open Logs.
- [ ] Verification: docs coverage audit, live authorized test-channel posts/capture/search/cleanup, full Go checks and CI including macOS, review, stacked PR.

Review focus: malformed author/text metadata; deeply nested content; raw bytes through archive rewrites; secret-bearing log attributes; unauthorized log access.
