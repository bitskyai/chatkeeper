# Cleanup settings implementation plan

User brief is the implementation spec. Extend existing workspace persistence and credential store; keep source-specific setup inside the source plugin via an optional interface. Preserve existing YAML policies and all archive/live/journal gates.

- [x] Add separate cleanup approval trigger, thread/messages scope and automatic interval to the neutral policy and strict config parser; test round-trip and approval gates.
- [x] Add plugin-owned member/emoji choices, OAuth links and credential validation (identity, workspace, privileges, scopes), with synthetic tests.
- [x] Add Settings form with secret-only writes, explicit zero-age acknowledgement, validated persistence, managed read-only mode and CSRF tests.
- [x] Offer isolated optional last wizard page; link cleanup reports to settings and show actionable reasons and eligibility time.
- [x] Add opt-in scheduled apply using bound previews and persistent progress logs.
- [x] Run unit/race/noslack/chromedp checks; live-test only tagged own bot posts; issue #31 opened; PR/CI verification follows publication; never merge.
