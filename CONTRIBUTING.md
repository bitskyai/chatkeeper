# Contributing

Issues and pull requests are welcome. Discuss changes to archive formats, plugin
contracts or security boundaries before implementation. Use synthetic fixtures;
never submit tokens, private messages, real workspace IDs or personal paths.

Use Go 1.26+, build with `go build ./cmd/chatkeeper`, then run:

```sh
go test -race ./...
go test -tags noslack ./...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
```

Browser tests need Chrome: `CHATKEEPER_E2E=1 go test ./internal/web -run TestBrowserWizard`.
Packaging checks are in `.github/workflows/ci.yml`. Include regression tests for
behavior changes and document compatibility implications. Plugins use
`pkg/pluginapi` and its contract test kit; see `docs/design/architecture.md`.
Contributions are licensed under the project's MIT license.
