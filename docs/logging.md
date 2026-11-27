# Logs and diagnostics

The server and capture CLI write JSON lines to `$CHATKEEPER_HOME/logs/chatkeeper.log` (the application data directory) and readable logs to stderr. Default home is the OS configuration directory plus `chatkeeper`: on macOS, `~/Library/Application Support/chatkeeper`; on Linux, `~/.config/chatkeeper`; containers should persist their configured `CHATKEEPER_HOME` volume. Cleanup writes to the same log.

Rotation keeps the active file plus five backups (`chatkeeper.log.1` through `.5`), at 10 MiB each. A shared file lock coordinates server, cleanup and backfill writers. Log files are 0600; directories are 0700. A record larger than 10 MiB may exceed the limit until the next write. Read-only MCP/verify/config commands do not create log state.

`CHATKEEPER_LOG_LEVEL` accepts DEBUG, INFO (default), WARN or ERROR for server/backfill. Logs include version and startup settings without credentials, source method results, retry delays, capture steps/results and cleanup results. The plugin credential redactor removes resolved secrets and credential-shaped strings from messages and structured values before either output. Do not log request/response bodies.

Open **Logs** in the administrator UI, or follow **Open persistent logs** from Check now. The page shows the latest 200 matching lines, filters by case-insensitive text (including JSON level fields), refreshes every five seconds, and downloads up to 100,000 matching lines from the bounded active file. Downloads do not include rotated backups. Sessions are required; API bearer access does not authorize logs.

```sh
chatkeeper logs
chatkeeper logs --filter error
chatkeeper logs --follow
```

Follow reopens the active log after rotation. If more than 2,000 matching records arrive between ticks, older records may be omitted; disk logs remain authoritative.

The macOS menu app separately captures child stdout/stderr in `~/Library/Logs/ChatKeeper/chatkeeper.log`, with the same 10 MiB × five-backup rotation. **Open Logs** reveals the file. **Copy diagnostics** copies app version, OS version and the last 200 redacted lines (reading at most 1 MiB) to the clipboard. Launch tokens and credential-shaped strings are removed. Server JSON logs remain in the application home above.
