# Slack message compatibility corpus

`testdata/slack/corpus/*.json` contains API-shaped synthetic responses and sanitized responses collected from a real bot session. The filename prefix is the category. Every file runs through a real plugin session, persistent archive save/read, HTML viewer and FTS search. A second pass replaces **every string value** with an XSS probe. The original transport bytes are asserted byte-for-byte independently of normalized rendering metadata. Invalid UTF-8 is intentionally present in one fixture.

Raw successful `conversations.replies` pages are stored as `raw/replies-<sha256>.json`. They are checksummed archive objects, retained across refreshes, edits and deletions. Message metadata used for indexing may have normalized timestamps, authors, text or reply counts; the original is always in the raw page. Unknown/bounded shapes produce per-message `unrendered` manifest gaps. Such gaps must block destructive cleanup. Corrupt archive checksums, unsafe keys, credentials/transport failures and nonterminating API pagination remain safety boundaries.

Run:

```sh
go test ./internal/plugins/slack -run TestFormatCorpus -count=1
go test ./internal/plugins/slack -run '^$' -fuzz FuzzMessageDecoder -fuzztime=10m -parallel=2 -timeout=12m
```

The decoder fuzz seeds include every corpus response. Fuzzing covers JSON decoding, bounded content extraction, display-name extraction and event normalization. It does not fuzz networking or the archive transaction machinery.

## Documentation audit (2026-11-27)

Cross-checked against Slack's [blocks](https://docs.slack.dev/reference/block-kit/blocks/), [elements](https://docs.slack.dev/reference/block-kit/block-elements/), [message subtypes](https://docs.slack.dev/reference/events/message/) and [rich text](https://docs.slack.dev/reference/block-kit/blocks/rich-text-block/) references (the former api.slack.com reference URLs redirect to these).

All message subtypes listed in the message reference have named fixtures, plus legacy/observed huddle, room, workflow/app, tombstone and Slackbot variants. Synthetic event wrappers test storage tolerance; they do not imply that Slack returns every Events API subtype from history/replies.

Blocks cover the requested rich text, section/fields/accessory, header, divider, image, context, actions, input, file, video, call, markdown, table and unknown type. Newer alert/card/carousel/container/context-actions/data-table/data-visualization/layout/plan/task-card shapes have generic forward-compatibility probes, not complete schema-specific renderers.

Elements cover buttons; all basic single/multi-select families; overflow; date/time/datetime pickers; checkboxes/radio; image; plain text/mrkdwn; emoji; rich/plain/email/URL/number/file inputs and workflow buttons. Rich text covers section, ordered/bullet lists, quote, preformatted, style map/string/null/missing, text styles, link, user/group/channel, emoji, broadcast, date/fallback, color, team and unknown.

**Gaps:** newer attachment mentions, canvas message unfurls/user mentions, citations, feedback/icon buttons, list records, Salesforce data fields, tags, URL sources, work-object/workflow mentions and composition-object combinations do not yet have dedicated schema-accurate fixtures. Generic unknown-type preservation applies. Block rendering is best-effort textual extraction, not Slack's interactive/visual layout. Synthetic block probes intentionally include irregular combinations; they are not all legal posting payloads. No finite corpus proves every future schema or combination.

## Live receipt

The actual CLI binary captured in polling-only mode with only a bot token. One successful pass posted 29 bot-owned test messages in 28 roots, verified all 28 viewers and returned all 28 roots through the MCP `search_threads` FTS query. The broadcast reply carried a skin-tone reaction; another message was edited with `chat.update`; `chat.meMessage` was exercised. Every created message was deleted successfully. The credential lived in a 0600 temporary file and was deleted. Captured raw responses were sanitized before being added as 28 `live-*` fixtures; no live credentials or workspace identifiers are committed.

Live variants: mrkdwn, Unicode/ZWJ/RTL/CJK, mentions, section/fields/button, header/divider/context, image, bullet/ordered lists, quote/preformatted, rich inline elements, button/static/external/user/conversation/channel selects, overflow/date/time/checkbox/radio actions, markdown/table, legacy/rich attachments, link-unfurl request, broadcast reply and me-message. Slack rejected a multi-select inside actions (`invalid_blocks`); file-upload scope probe returned `missing_scope`, so no file was uploaded. Input/file/video/call blocks, newer blocks, app-specific workflow/assistant posts, admin/system subtypes, genuine deletion/retention/EKM states and all real unfurl service responses were not generated live. Unfurl posting was requested, but service-side unfurl generation is asynchronous and not asserted.
